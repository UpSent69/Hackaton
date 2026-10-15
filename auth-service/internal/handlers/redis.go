package handlers

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// KV — Redis рядом с SQLite. В SQLite лежат основные данные склада, в Redis — временные:
// сессии, кэш ответов, счётчик изменений, ограничение частоты запросов, состояние анализа
// Qwen и очередь распознавания документов.
//
// Redis необязателен: если он недоступен, каждый вызов сразу возвращает «нет», и код идёт
// запасным путём (SQLite или память процесса). Раз в 3 секунды соединение проверяется;
// когда Redis возвращается, временные ключи сбрасываются и заново наполняются из SQLite —
// так в нём не остаётся, например, сессии, из которой вышли, пока Redis был недоступен.
type KV struct {
	client  *redis.Client
	healthy atomic.Bool
}

const kvTimeout = 300 * time.Millisecond

func NewKV(addr string) *KV {
	kv := &KV{}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		log.Println("Redis: REDIS_ADDR не задан — временные данные хранятся в SQLite и памяти процесса")
		return kv
	}
	kv.client = redis.NewClient(&redis.Options{
		Addr: addr, DialTimeout: kvTimeout, ReadTimeout: 5 * time.Second, WriteTimeout: kvTimeout,
		MaxRetries: -1,
	})
	kv.check()
	go func() {
		for range time.Tick(3 * time.Second) {
			kv.check()
		}
	}()
	return kv
}

func (kv *KV) check() {
	ctx, cancel := context.WithTimeout(context.Background(), kvTimeout)
	defer cancel()
	ok := kv.client.Ping(ctx).Err() == nil
	was := kv.healthy.Swap(ok)
	switch {
	case ok && !was:
		kv.resetVolatile()
		log.Printf("Redis: подключён (%s)", kv.client.Options().Addr)
	case !ok && was:
		log.Printf("Redis: недоступен (%s) — работаем без него", kv.client.Options().Addr)
	}
}

// resetVolatile удаляет ключи, которые могли устареть, пока Redis был недоступен.
func (kv *KV) resetVolatile() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, pattern := range []string{"session:*", "user-sessions:*", "cache:*", "alerts:*", "rev"} {
		iter := kv.client.Scan(ctx, 0, pattern, 200).Iterator()
		for iter.Next(ctx) {
			kv.client.Del(ctx, iter.Val())
		}
	}
}

// Ready — можно ли сейчас обращаться к Redis.
func (kv *KV) Ready() bool { return kv != nil && kv.client != nil && kv.healthy.Load() }

func (kv *KV) fail(err error) {
	if err != nil && !errors.Is(err, redis.Nil) && kv.healthy.Swap(false) {
		log.Printf("Redis: ошибка %v — работаем без него до восстановления", err)
	}
}

func (kv *KV) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), kvTimeout)
}

func (kv *KV) Get(key string) (string, bool) {
	if !kv.Ready() {
		return "", false
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	value, err := kv.client.Get(ctx, key).Result()
	kv.fail(err)
	return value, err == nil
}

func (kv *KV) Set(key, value string, ttl time.Duration) bool {
	if !kv.Ready() {
		return false
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	err := kv.client.Set(ctx, key, value, ttl).Err()
	kv.fail(err)
	return err == nil
}

// SetNX — записать, только если ключа нет (замок для фоновой задачи).
func (kv *KV) SetNX(key, value string, ttl time.Duration) (acquired, ok bool) {
	if !kv.Ready() {
		return false, false
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	acquired, err := kv.client.SetNX(ctx, key, value, ttl).Result()
	kv.fail(err)
	return acquired, err == nil
}

func (kv *KV) Del(keys ...string) bool {
	if !kv.Ready() || len(keys) == 0 {
		return false
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	err := kv.client.Del(ctx, keys...).Err()
	kv.fail(err)
	return err == nil
}

func (kv *KV) SAdd(key, member string, ttl time.Duration) {
	if !kv.Ready() {
		return
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	pipe := kv.client.TxPipeline()
	pipe.SAdd(ctx, key, member)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	kv.fail(err)
}

func (kv *KV) SMembers(key string) []string {
	if !kv.Ready() {
		return nil
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	members, err := kv.client.SMembers(ctx, key).Result()
	kv.fail(err)
	return members
}

func (kv *KV) RPush(key, value string) bool {
	if !kv.Ready() {
		return false
	}
	ctx, cancel := kv.ctx()
	defer cancel()
	err := kv.client.RPush(ctx, key, value).Err()
	kv.fail(err)
	return err == nil
}

// BLPop ждёт элемент очереди не дольше wait.
func (kv *KV) BLPop(key string, wait time.Duration) (string, bool) {
	if !kv.Ready() {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait+time.Second)
	defer cancel()
	values, err := kv.client.BLPop(ctx, wait, key).Result()
	kv.fail(err)
	if err != nil || len(values) != 2 {
		return "", false
	}
	return values[1], true
}

/* ---------- Ограничение частоты запросов ---------- */

var (
	memoryLimitsMu sync.Mutex
	memoryLimits   = make(map[string]memoryLimit)
)

type memoryLimit struct {
	count int
	reset time.Time
}

// allow — не больше limit запросов за window. В Redis: INCR + EXPIRE, без Redis — счётчик в памяти.
func (a *API) allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	key = "ratelimit:" + key
	if a.KV.Ready() {
		ctx, cancel := a.KV.ctx()
		defer cancel()
		count, err := a.KV.client.Incr(ctx, key).Result()
		if err == nil {
			if count == 1 {
				a.KV.client.Expire(ctx, key, window)
			}
			if count > int64(limit) {
				ttl, _ := a.KV.client.TTL(ctx, key).Result()
				if ttl <= 0 {
					ttl = window
				}
				return false, ttl
			}
			return true, 0
		}
		a.KV.fail(err)
	}
	memoryLimitsMu.Lock()
	defer memoryLimitsMu.Unlock()
	now := time.Now()
	item := memoryLimits[key]
	if now.After(item.reset) {
		item = memoryLimit{reset: now.Add(window)}
	}
	item.count++
	memoryLimits[key] = item
	if item.count > limit {
		return false, item.reset.Sub(now)
	}
	return true, 0
}

// rateLimited — обёртка маршрута: ключ строится по адресу клиента или по пользователю.
func (a *API) rateLimited(name string, limit int, window time.Duration, byUser bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who := clientIP(r)
		if byUser {
			if auth := currentAuth(r); auth != nil {
				who = "user:" + auth.User.ID
			}
		}
		if ok, wait := a.allow(name+":"+who, limit, window); !ok {
			seconds := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			a.writeError(w, http.StatusTooManyRequests, "Слишком много запросов. Попробуйте снова через "+strconv.Itoa(seconds)+" с.")
			return
		}
		next(w, r)
	}
}

// clientIP — адрес клиента. Бэкенд стоит за nginx фронтенда, который передаёт X-Real-IP.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

/* ---------- Кэш ответов ---------- */

const responseCacheTTL = 10 * time.Second

// cachedJSON кэширует ответ GET-маршрута в Redis. Ключ включает ревизию данных, поэтому любое
// изменение склада сразу даёт новый ключ; короткий срок жизни покрывает то, что меняется со
// временем (сроки годности). Ответ одинаков для всех пользователей.
func (a *API) cachedJSON(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.KV.Ready() {
			next(w, r)
			return
		}
		key := "cache:" + name + ":" + strconv.FormatInt(a.currentRev(), 10) + ":" + r.URL.RawQuery
		if body, ok := a.KV.Get(key); ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		recorder := httptest.NewRecorder()
		next(recorder, r)
		if recorder.Code == http.StatusOK {
			a.KV.Set(key, recorder.Body.String(), responseCacheTTL)
		}
		for header, values := range recorder.Header() {
			w.Header()[header] = values
		}
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}
}
