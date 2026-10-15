package handlers

import (
	"auth-service/internal/requestimport"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Очередь распознавания документов (накладные во вкладке «Поставки», заявки во вкладке
// «Заявка»). Распознавание через Qwen может занять больше минуты, а nginx держит запрос
// не дольше 60 секунд, поэтому файл ставится в очередь: маршрут сразу отвечает 202 с номером
// задачи, фоновый обработчик распознаёт документ, а страница забирает результат через
// GET /api/jobs/{id}. Задачи и результаты хранятся в Redis (переживают перезапуск сервера),
// без Redis — в памяти процесса. Обработчик один: Ollama всё равно отвечает по очереди.

const (
	jobQueueKey = "jobs:documents"
	jobTTL      = time.Hour
	jobTimeout  = 3 * time.Minute
)

type documentJob struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // "supply" или "request"
	Filename string `json:"filename"`
	Hash     string `json:"hash"`
	Text     string `json:"text"`
	Image    []byte `json:"image"`
	MIME     string `json:"mime"`
}

type jobResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Запасное хранилище задач, пока Redis недоступен.
var (
	memoryJobsMu    sync.Mutex
	memoryJobs      = make(map[string]*memoryJob)
	memoryJobsQueue = make(chan string, 200)
)

type memoryJob struct {
	job    documentJob
	result *jobResult
	at     time.Time
}

func newJobID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// enqueueDocument ставит документ в очередь и отвечает 202 с номером задачи.
func (a *API) enqueueDocument(w http.ResponseWriter, kind, filename, hash string, source requestimport.Source) {
	job := documentJob{ID: newJobID(), Kind: kind, Filename: filename, Hash: hash, Text: source.Text, Image: source.Image, MIME: source.ImageMIME}
	encoded, err := json.Marshal(job)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось поставить документ в очередь")
		return
	}
	if !(a.KV.Set("job:"+job.ID, string(encoded), jobTTL) && a.KV.RPush(jobQueueKey, job.ID)) {
		memoryJobsMu.Lock()
		memoryJobs[job.ID] = &memoryJob{job: job, at: time.Now()}
		memoryJobsMu.Unlock()
		select {
		case memoryJobsQueue <- job.ID:
		default:
			a.writeError(w, http.StatusServiceUnavailable, "Очередь распознавания переполнена, попробуйте через минуту")
			return
		}
	}
	log.Printf("Очередь документов: задача %s (%s, %s) поставлена", job.ID, kind, filename)
	a.sendJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "jobId": job.ID})
}

// Job отдаёт результат задачи: 202 — ещё распознаётся, иначе статус и тело ответа распознавания.
func (a *API) Job(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	memoryJobsMu.Lock()
	entry := memoryJobs[id]
	var result *jobResult
	if entry != nil {
		result = entry.result
	}
	memoryJobsMu.Unlock()

	if entry == nil {
		if raw, ok := a.KV.Get("job:" + id + ":result"); ok {
			result = &jobResult{}
			if json.Unmarshal([]byte(raw), result) != nil {
				result = nil
			}
		} else if _, ok := a.KV.Get("job:" + id); !ok {
			a.writeError(w, http.StatusNotFound, "Задача не найдена или устарела — загрузите файл заново")
			return
		}
	}
	if result == nil {
		a.sendJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "jobId": id})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

// startDocumentWorker запускает фоновый обработчик очереди.
func (a *API) startDocumentWorker() {
	go func() {
		for {
			if id, ok := a.nextJob(); ok {
				a.runJob(id)
			}
		}
	}()
	go func() { // устаревшие задачи из памяти удаляются через час
		for range time.Tick(10 * time.Minute) {
			memoryJobsMu.Lock()
			for id, entry := range memoryJobs {
				if time.Since(entry.at) > jobTTL {
					delete(memoryJobs, id)
				}
			}
			memoryJobsMu.Unlock()
		}
	}()
}

func (a *API) nextJob() (string, bool) {
	select {
	case id := <-memoryJobsQueue:
		return id, true
	default:
	}
	if a.KV.Ready() {
		return a.KV.BLPop(jobQueueKey, 2*time.Second)
	}
	select {
	case id := <-memoryJobsQueue:
		return id, true
	case <-time.After(2 * time.Second):
		return "", false
	}
}

func (a *API) runJob(id string) {
	memoryJobsMu.Lock()
	entry := memoryJobs[id]
	memoryJobsMu.Unlock()
	var job documentJob
	if entry != nil {
		job = entry.job
	} else if raw, ok := a.KV.Get("job:" + id); !ok || json.Unmarshal([]byte(raw), &job) != nil {
		log.Printf("Очередь документов: задача %s не найдена", id)
		return
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	recorder := httptest.NewRecorder()
	source := requestimport.Source{Text: job.Text, Image: job.Image, ImageMIME: job.MIME}
	if job.Kind == "supply" {
		a.finishSupplyParse(ctx, recorder, job.Filename, job.Hash, source)
	} else {
		a.finishRequestParse(ctx, recorder, job.Filename, job.Hash, source)
	}
	result := jobResult{Status: recorder.Code, Body: json.RawMessage(recorder.Body.Bytes())}
	log.Printf("Очередь документов: задача %s готова за %s, HTTP %d", id, time.Since(started).Round(time.Second), result.Status)

	if entry != nil {
		memoryJobsMu.Lock()
		entry.result = &result
		memoryJobsMu.Unlock()
		return
	}
	encoded, _ := json.Marshal(result)
	if !a.KV.Set("job:"+id+":result", string(encoded), jobTTL) {
		// Redis пропал во время распознавания — результат сохраняется в памяти.
		memoryJobsMu.Lock()
		memoryJobs[id] = &memoryJob{job: job, result: &result, at: time.Now()}
		memoryJobsMu.Unlock()
	}
}
