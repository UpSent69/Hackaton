package handlers

import (
	"auth-service/internal/contract"
	"auth-service/internal/events"
	"auth-service/internal/requestimport"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const authContextKey contextKey = "stoplist_auth"

type authContext struct {
	User    contract.User
	TokenID string
	// RestaurantID — ресторан, в котором выполняется запрос: у владельца и поставщика —
	// выбранный (заголовок X-Restaurant-Id), у остальных — их собственный. 0 — не выбран.
	RestaurantID int64
}

// restaurantOf — ресторан текущего запроса.
func restaurantOf(r *http.Request) int64 {
	if auth := currentAuth(r); auth != nil {
		return auth.RestaurantID
	}
	return 0
}

// restaurantFree — маршруты, которым не нужен выбранный ресторан: вход, сотрудники,
// список ресторанов, общие часы демо, результаты задач. Поставки поставщик видит и создаёт
// сразу для всех ресторанов (ресторан указывается в самой поставке).
var restaurantFree = map[string]bool{
	"POST /api/auth/logout": true, "GET /api/auth/me": true,
	"GET /api/users": true, "POST /api/users": true, "DELETE /api/users/{id}": true,
	"GET /api/restaurants": true, "GET /api/jobs/{id}": true, "GET /api/pulse": true,
	"GET /api/supplies": true, "POST /api/supplies": true,
	"GET /api/demo/state": true, "POST /api/demo/clock": true, "POST /api/demo/advance": true,
}

// The warehouse demo is intentionally serialized. SQLite has one connection and this
// also ensures that two orders cannot both spend the same final batch quantity.
var requestMu sync.Mutex

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		a.sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Подбор пароля и массовые регистрации ограничены по адресу клиента.
	mux.HandleFunc("POST /api/auth/register", a.rateLimited("register", 5, time.Minute, false, a.Register))
	mux.HandleFunc("POST /api/auth/login", a.rateLimited("login", 10, time.Minute, false, a.Login))
	mux.HandleFunc("GET /api/auth/registration", a.Registration)

	// extra — роли сотрудников (официант, поставщик), которым маршрут открыт дополнительно:
	// владельцу и директору доступно всё, менеджеру — всё, что не adminOnly, остальным — только extra.
	authed := func(method, path string, admin bool, handler http.HandlerFunc, extra ...contract.Role) {
		mux.Handle(method+" "+path, a.protect(admin, handler, extra...))
	}
	waiter, supplier := contract.RoleWaiter, contract.RoleSupplier

	authed("POST", "/api/auth/logout", false, a.Logout, waiter, supplier)
	authed("GET", "/api/auth/me", false, a.Me, waiter, supplier)
	authed("GET", "/api/users", true, a.Users)
	authed("GET", "/api/restaurants", false, a.Restaurants, waiter, supplier)
	authed("POST", "/api/users", true, a.CreateManager)
	authed("DELETE", "/api/users/{id}", true, a.DeleteManager)

	// Каталог, остатки, меню, горячие предложения и аналитика кэшируются в Redis по ревизии данных.
	authed("GET", "/api/products", false, a.cachedJSON("products", a.Products), supplier)
	authed("POST", "/api/products/{productId}/price", true, a.UpdateProductPrice)
	authed("GET", "/api/requests", false, a.Requests)
	authed("GET", "/api/requests/{id}", false, a.RequestByID)
	authed("POST", "/api/requests", true, a.CreateRequest)
	mux.Handle("POST /api/requests/parse", a.protectLongRunning(false, a.rateLimited("parse", 10, time.Minute, true, a.ParseRequestFile)))
	authed("GET", "/api/jobs/{id}", false, a.Job, supplier)
	authed("GET", "/api/stock", false, a.cachedJSON("stock", a.Stock))
	authed("GET", "/api/stock/{productId}/batches", false, a.Batches)
	authed("GET", "/api/expiry", false, a.Expiry)
	authed("POST", "/api/batches/{id}/writeoff", true, a.WriteOffBatch)
	authed("GET", "/api/supplies", false, a.Supplies, supplier)
	authed("POST", "/api/supplies", true, a.CreateSupply, supplier)
	mux.Handle("POST /api/supplies/parse", a.protectLongRunning(true, a.rateLimited("parse", 10, time.Minute, true, a.ParseSupplyFile), supplier))
	authed("POST", "/api/supplies/{id}/review", true, a.ReviewSupply, contract.RoleManager)
	authed("DELETE", "/api/supplies/{id}", true, a.DeleteSupply)
	authed("DELETE", "/api/supplies/{id}/items/{productId}", true, a.DeleteSupplyItem)

	authed("GET", "/api/menu", false, a.cachedJSON("menu", a.Menu), waiter)
	authed("POST", "/api/dishes", true, a.CreateDish)
	authed("DELETE", "/api/dishes/{id}", true, a.DeleteDish)
	authed("POST", "/api/dishes/{id}/price", true, a.UpdateDishPrice)
	authed("POST", "/api/orders/validate", false, a.ValidateOrder, waiter)
	authed("POST", "/api/orders", true, a.CreateOrder, waiter)
	authed("GET", "/api/orders", false, a.Orders, waiter)

	authed("GET", "/api/pulse", false, a.Pulse, waiter, supplier)
	authed("GET", "/api/hot-offers", false, a.cachedJSON("hot-offers", a.HotOffers))
	authed("GET", "/api/analytics", false, a.cachedJSON("analytics", a.Analytics))
	mux.Handle("GET /api/analytics/ai", a.protectLongRunning(false, a.AnalyticsInsight))
	// Результаты распознавания и анализа приходят из ai-service через Kafka.
	a.Bus.Consume(events.TopicDocumentResults, "core", a.storeDocumentResult)
	a.Bus.Consume(events.TopicAnalyticsResults, "core", a.storeInsightResult)
	go cleanupMemoryJobs()

	authed("GET", "/api/demo/state", false, a.DemoState)
	authed("POST", "/api/demo/clock", true, a.DemoClock)
	authed("POST", "/api/demo/advance", true, a.DemoAdvance)
	authed("POST", "/api/demo/seed", true, a.DemoSeed)
	authed("POST", "/api/demo/restock", true, a.DemoRestock)
	authed("POST", "/api/demo/reset", true, a.DemoReset)
}

// roleAllowed: владельцу и директору доступно всё (директору — в своём ресторане), менеджеру —
// маршруты без adminOnly, официанту и поставщику — только маршруты, где их роль указана явно.
func roleAllowed(role contract.Role, adminOnly bool, extra []contract.Role) bool {
	if role == contract.RoleOwner || role == contract.RoleDirector {
		return true
	}
	for _, allowed := range extra {
		if role == allowed {
			return true
		}
	}
	return role == contract.RoleManager && !adminOnly
}

// validRole — роль, с которой можно войти.
func validRole(role string) bool {
	switch contract.Role(role) {
	case contract.RoleOwner, contract.RoleDirector, contract.RoleManager, contract.RoleWaiter, contract.RoleSupplier:
		return true
	}
	return false
}

// isHead — владелец или директор: полные права в ресторане запроса.
func isHead(role contract.Role) bool {
	return role == contract.RoleOwner || role == contract.RoleDirector
}

// authorize проверяет вход, права и ресторан запроса. Вызывается под requestMu.
func (a *API) authorize(w http.ResponseWriter, r *http.Request, adminOnly bool, extra []contract.Role) (*authContext, bool) {
	auth, err := a.authenticate(r)
	if err != nil {
		a.writeError(w, http.StatusUnauthorized, "Требуется вход или срок сессии истёк")
		return nil, false
	}
	if !roleAllowed(auth.User.Role, adminOnly, extra) {
		a.writeError(w, http.StatusForbidden, "Недостаточно прав для выполнения операции")
		return nil, false
	}
	auth.RestaurantID = a.requestRestaurant(r, auth.User)
	if auth.RestaurantID == 0 && !restaurantFree[r.Pattern] {
		a.writeError(w, http.StatusBadRequest, "Выберите ресторан")
		return nil, false
	}
	if err := a.maintainIfOwner(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось обновить состояние склада")
		return nil, false
	}
	return auth, true
}

// requestRestaurant: владелец и поставщик выбирают ресторан заголовком X-Restaurant-Id,
// остальные всегда работают в своём ресторане (заголовок игнорируется).
func (a *API) requestRestaurant(r *http.Request, user contract.User) int64 {
	if user.Role != contract.RoleOwner && user.Role != contract.RoleSupplier {
		id, _ := strconv.ParseInt(user.RestaurantID, 10, 64)
		return id
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Restaurant-Id")), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	var exists int
	if a.DB.QueryRow("SELECT COUNT(*) FROM restaurants WHERE id=?", id).Scan(&exists) != nil || exists == 0 {
		return 0
	}
	return id
}

func (a *API) protect(adminOnly bool, next http.HandlerFunc, extra ...contract.Role) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()
		auth, ok := a.authorize(w, r, adminOnly, extra)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), authContextKey, auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// protectLongRunning authenticates under the SQLite request lock, then releases it before
// CPU/network-heavy work such as AI extraction. The handler only reads the catalogue afterwards.
func (a *API) protectLongRunning(adminOnly bool, next http.HandlerFunc, extra ...contract.Role) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		auth, ok := a.authorize(w, r, adminOnly, extra)
		requestMu.Unlock()
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), authContextKey, auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) authenticate(r *http.Request) (*authContext, error) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return nil, errors.New("missing bearer token")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(parts[1], claims, func(token *jwt.Token) (interface{}, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		if len(jwtSecret) < 32 {
			return nil, errors.New("JWT secret is not configured")
		}
		return jwtSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || token == nil || !token.Valid {
		return nil, errors.New("invalid token")
	}

	idValue, ok := claims["user_id"].(float64)
	if !ok || idValue <= 0 {
		return nil, errors.New("invalid user claim")
	}
	tokenID, ok := claims["jti"].(string)
	if !ok || tokenID == "" {
		return nil, errors.New("invalid session claim")
	}

	// Быстрый путь: сессия в Redis (живёт до истечения токена). Пользователь, роль и ресторан
	// всё равно читаются из SQLite, чтобы изменения действовали сразу.
	id := int64(idValue)
	if uid, ok := a.KV.Get("session:" + tokenID); ok && uid == strconv.FormatInt(id, 10) {
		if user, err := a.loadUser(id); err == nil {
			return &authContext{User: user, TokenID: tokenID}, nil
		}
	}

	var expiresAt string
	var revoked int
	err = a.DB.QueryRow("SELECT expires_at, revoked FROM auth_sessions WHERE user_id = ? AND jti = ?", id, tokenID).Scan(&expiresAt, &revoked)
	if err != nil || revoked != 0 {
		return nil, errors.New("session not found")
	}
	expiration, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !time.Now().UTC().Before(expiration) {
		return nil, errors.New("session expired")
	}
	user, err := a.loadUser(id)
	if err != nil {
		return nil, err
	}
	a.rememberSession(tokenID, id, time.Until(expiration))
	return &authContext{User: user, TokenID: tokenID}, nil
}

// loadUser читает аккаунт вместе с его рестораном.
func (a *API) loadUser(id int64) (contract.User, error) {
	var user contract.User
	var role string
	var restaurantID sql.NullInt64
	var restaurantName sql.NullString
	err := a.DB.QueryRow(
		"SELECT a.id,a.name,a.email,a.role,a.restaurant_id,r.name FROM accounts a "+
			"LEFT JOIN restaurants r ON r.id=a.restaurant_id WHERE a.id=?", id,
	).Scan(&id, &user.Name, &user.Email, &role, &restaurantID, &restaurantName)
	if err != nil {
		return user, err
	}
	if !validRole(role) {
		return user, errors.New("invalid role")
	}
	user.ID = strconv.FormatInt(id, 10)
	user.Role = contract.Role(role)
	if restaurantID.Valid {
		user.RestaurantID = strconv.FormatInt(restaurantID.Int64, 10)
		user.RestaurantName = restaurantName.String
	}
	return user, nil
}

func currentAuth(r *http.Request) *authContext {
	auth, _ := r.Context().Value(authContextKey).(*authContext)
	return auth
}

func (a *API) Register(w http.ResponseWriter, r *http.Request) {
	requestMu.Lock()
	defer requestMu.Unlock()

	var req contract.RegisterRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте формат данных регистрации")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Name == "" || len(req.Name) > 120 {
		a.writeError(w, http.StatusBadRequest, "Укажите имя длиной до 120 символов")
		return
	}
	if len(req.Email) > 254 {
		a.writeError(w, http.StatusBadRequest, "Укажите корректную почту")
		return
	}
	if address, err := mail.ParseAddress(req.Email); err != nil || address.Address != req.Email {
		a.writeError(w, http.StatusBadRequest, "Укажите корректную почту")
		return
	}
	if len(req.Password) < 6 {
		a.writeError(w, http.StatusBadRequest, "Пароль должен содержать не менее 6 символов")
		return
	}
	if len(req.Password) > 72 {
		a.writeError(w, http.StatusBadRequest, "Пароль слишком длинный")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать пароль")
		return
	}
	// Самостоятельно регистрируются только владельцы сети — первые два аккаунта.
	// Директоров, сотрудников и поставщика заводят на странице «Администрирование».
	open, err := a.registrationOpen()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить пользователей")
		return
	}
	if !open {
		a.writeError(w, http.StatusForbidden, "Регистрация закрыта: аккаунты выдаёт владелец или директор ресторана")
		return
	}
	role := contract.RoleOwner
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := a.DB.Exec(
		"INSERT INTO accounts(name,email,password_hash,role,created_at) VALUES(?,?,?,?,?)",
		req.Name, req.Email, string(hash), string(role), createdAt,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			a.writeError(w, http.StatusConflict, "Аккаунт с такой почтой уже существует")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать аккаунт")
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать аккаунт")
		return
	}
	user := contract.User{ID: strconv.FormatInt(id, 10), Name: req.Name, Email: req.Email, Role: role}
	session, err := a.createSession(id, user)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Аккаунт создан, но не удалось открыть сессию")
		return
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusOK, session)
}

func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	requestMu.Lock()
	defer requestMu.Unlock()

	var req contract.LoginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте формат данных входа")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	var id int64
	var name, storedEmail, passwordHash, role string
	err := a.DB.QueryRow(
		"SELECT id,name,email,password_hash,role FROM accounts WHERE email = ? COLLATE NOCASE",
		email,
	).Scan(&id, &name, &storedEmail, &passwordHash, &role)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)) != nil {
		a.writeError(w, http.StatusUnauthorized, "Логин или пароль неверны")
		return
	}
	user, err := a.loadUser(id)
	if err != nil {
		a.writeError(w, http.StatusUnauthorized, "Логин или пароль неверны")
		return
	}
	session, err := a.createSession(id, user)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось открыть сессию")
		return
	}
	a.sendJSON(w, http.StatusOK, session)
}

func (a *API) createSession(userID int64, user contract.User) (contract.Session, error) {
	randomID := make([]byte, 24)
	if _, err := rand.Read(randomID); err != nil {
		return contract.Session{}, err
	}
	jti := hex.EncodeToString(randomID)
	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	claims := jwt.MapClaims{
		"user_id": userID,
		"role": string(user.Role),
		"jti": jti,
		"iat": now.Unix(),
		"exp": expires.Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return contract.Session{}, err
	}
	if _, err := a.DB.Exec(
		"INSERT INTO auth_sessions(jti,user_id,expires_at,revoked) VALUES(?,?,?,0)",
		jti, userID, expires.Format(time.RFC3339Nano),
	); err != nil {
		return contract.Session{}, err
	}
	a.rememberSession(jti, userID, expires.Sub(now))
	return contract.Session{Token: tokenString, User: user}, nil
}

// rememberSession кладёт сессию в Redis со сроком жизни токена. Основная запись остаётся
// в SQLite (auth_sessions), поэтому без Redis вход продолжает работать.
func (a *API) rememberSession(jti string, userID int64, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	uid := strconv.FormatInt(userID, 10)
	if a.KV.Set("session:"+jti, uid, ttl) {
		a.KV.SAdd("user-sessions:"+uid, jti, 25*time.Hour)
	}
}

// forgetSessions удаляет из Redis все сессии пользователя (удаление менеджера).
func (a *API) forgetSessions(userID int64) {
	key := "user-sessions:" + strconv.FormatInt(userID, 10)
	keys := []string{key}
	for _, jti := range a.KV.SMembers(key) {
		keys = append(keys, "session:"+jti)
	}
	a.KV.Del(keys...)
}

func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	if auth != nil {
		_, _ = a.DB.Exec("UPDATE auth_sessions SET revoked=1 WHERE jti=?", auth.TokenID)
		a.KV.Del("session:" + auth.TokenID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) Me(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	if auth == nil {
		a.writeError(w, http.StatusUnauthorized, "Требуется вход")
		return
	}
	a.sendJSON(w, http.StatusOK, auth.User)
}

// Users — владелец видит все аккаунты сети, директор — сотрудников своего ресторана.
func (a *API) Users(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	query := "SELECT a.id FROM accounts a"
	args := []any{}
	if auth.User.Role != contract.RoleOwner {
		query += " WHERE a.restaurant_id=?"
		args = append(args, auth.User.RestaurantID)
	}
	rows, err := a.DB.Query(query+" ORDER BY a.restaurant_id IS NOT NULL, a.restaurant_id, a.name COLLATE NOCASE", args...)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить пользователей")
		return
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать пользователей")
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	users := make([]contract.User, 0, len(ids))
	for _, id := range ids {
		user, err := a.loadUser(id)
		if err != nil {
			continue
		}
		users = append(users, user)
	}
	a.sendJSON(w, http.StatusOK, users)
}

// Restaurants — рестораны, доступные пользователю: владельцу и поставщику — все, остальным — свой.
func (a *API) Restaurants(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	query, args := "SELECT id,name FROM restaurants ORDER BY id", []any{}
	if auth.User.Role != contract.RoleOwner && auth.User.Role != contract.RoleSupplier {
		query, args = "SELECT id,name FROM restaurants WHERE id=?", []any{auth.User.RestaurantID}
	}
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить рестораны")
		return
	}
	defer rows.Close()
	result := make([]contract.Restaurant, 0)
	for rows.Next() {
		var id int64
		var item contract.Restaurant
		if err := rows.Scan(&id, &item.Name); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать рестораны")
			return
		}
		item.ID = strconv.FormatInt(id, 10)
		result = append(result, item)
	}
	a.sendJSON(w, http.StatusOK, result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected data after JSON body")
	}
	return nil
}

func (a *API) writeError(w http.ResponseWriter, status int, message string) {
	a.sendJSON(w, status, contract.Error{Message: message})
}

func (a *API) writeErrorWithShortages(w http.ResponseWriter, status int, message string, shortages []contract.Shortage) {
	a.sendJSON(w, status, contract.Error{Message: message, Shortages: shortages})
}

func parseID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid identifier")
	}
	return id, nil
}

func normalizeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "ё", "е")
	return strings.Join(strings.Fields(value), " ")
}

func validUnit(unit contract.Unit) bool {
	return unit == contract.UnitGram || unit == contract.UnitMilliliter || unit == contract.UnitPiece
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (a *API) RevisionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.Header().Set("X-Data-Rev", strconv.FormatInt(a.currentRev(), 10))
		status := recorder.Code
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(recorder.Body.Bytes())
	})
}

// currentRev — ревизия данных. Её спрашивает каждый ответ (X-Data-Rev) и опрос страниц раз
// в 3 секунды, поэтому она читается из Redis; источник истины — SQLite (app_meta).
func (a *API) currentRev() int64 {
	if cached, ok := a.KV.Get("rev"); ok {
		if rev, err := strconv.ParseInt(cached, 10, 64); err == nil {
			return rev
		}
	}
	var value string
	if err := a.DB.QueryRow("SELECT value FROM app_meta WHERE key='revision'").Scan(&value); err != nil {
		return 0
	}
	rev, _ := strconv.ParseInt(value, 10, 64)
	a.KV.Set("rev", value, 0)
	return rev
}

func (a *API) bumpRev() {
	_, _ = a.DB.Exec("UPDATE app_meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")
	// Следующее чтение возьмёт новое значение из SQLite; вместе с ним меняются и ключи кэша.
	a.KV.Del("rev")
}

func (a *API) nowAndSpeed() (time.Time, float64) {
	var realBase, simBase, speedValue string
	err := a.DB.QueryRow(
		"SELECT " +
			"(SELECT value FROM app_meta WHERE key='clock_base_real')," +
			"(SELECT value FROM app_meta WHERE key='clock_base_sim')," +
			"(SELECT value FROM app_meta WHERE key='clock_speed')",
	).Scan(&realBase, &simBase, &speedValue)
	if err != nil {
		return time.Now().UTC(), 1
	}
	realTime, realErr := time.Parse(time.RFC3339Nano, realBase)
	simTime, simErr := time.Parse(time.RFC3339Nano, simBase)
	speed, speedErr := strconv.ParseFloat(speedValue, 64)
	if realErr != nil || simErr != nil || speedErr != nil || speed <= 0 {
		return time.Now().UTC(), 1
	}
	elapsed := time.Since(realTime)
	shift := time.Duration(float64(elapsed) * speed)
	return simTime.Add(shift).UTC(), speed
}

func (a *API) setClock(simulated time.Time, speed float64) error {
	now := time.Now().UTC()
	_, err := a.DB.Exec(
		"UPDATE app_meta SET value=CASE key " +
			"WHEN 'clock_base_real' THEN ? WHEN 'clock_base_sim' THEN ? WHEN 'clock_speed' THEN ? ELSE value END " +
			"WHERE key IN ('clock_base_real','clock_base_sim','clock_speed')",
		formatTime(now), formatTime(simulated), strconv.FormatFloat(speed, 'f', -1, 64),
	)
	if err == nil {
		a.bumpRev()
	}
	return err
}

func (a *API) shifted() bool {
	now, speed := a.nowAndSpeed()
	return speed != 1 || now.Sub(time.Now().UTC()) > time.Second || now.Sub(time.Now().UTC()) < -time.Second
}

func jsonString(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func safeRowsErr(rows *sql.Rows) error {
	if rows == nil {
		return nil
	}
	return rows.Err()
}

var managerLoginPattern = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)

// registrationOpen — регистрация открыта, пока владельцев меньше двух: владельцами
// становятся первые два зарегистрированных аккаунта.
func (a *API) registrationOpen() (bool, error) {
	var owners int
	err := a.DB.QueryRow("SELECT COUNT(*) FROM accounts WHERE role=?", string(contract.RoleOwner)).Scan(&owners)
	return owners < 2, err
}

// Registration сообщает форме входа, показывать ли ссылку на регистрацию.
func (a *API) Registration(w http.ResponseWriter, r *http.Request) {
	requestMu.Lock()
	defer requestMu.Unlock()
	open, err := a.registrationOpen()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить регистрацию")
		return
	}
	a.sendJSON(w, http.StatusOK, contract.RegistrationState{Open: open})
}

// CreateManager заводит аккаунт по логину и паролю. Владелец создаёт директоров (по одному
// на ресторан), менеджеров и официантов любого ресторана и общего поставщика; директор —
// менеджеров и официантов своего ресторана. Логин хранится в поле почты, вход — через ту же форму.
func (a *API) CreateManager(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	var req contract.CreateManagerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Укажите логин и пароль")
		return
	}
	login := strings.ToLower(strings.TrimSpace(req.Login))
	if !managerLoginPattern.MatchString(login) {
		a.writeError(w, http.StatusBadRequest, "Логин: от 3 до 64 символов — латиница, цифры, точка, дефис или подчёркивание")
		return
	}
	if len(req.Password) < 6 {
		a.writeError(w, http.StatusBadRequest, "Пароль должен содержать не менее 6 символов")
		return
	}
	if len(req.Password) > 72 {
		a.writeError(w, http.StatusBadRequest, "Пароль слишком длинный")
		return
	}
	role := req.Role
	if role == "" {
		role = contract.RoleManager
	}
	owner := auth.User.Role == contract.RoleOwner
	switch {
	case role == contract.RoleManager || role == contract.RoleWaiter:
	case owner && (role == contract.RoleDirector || role == contract.RoleSupplier):
	case owner:
		a.writeError(w, http.StatusBadRequest, "Роль: директор, менеджер, официант или поставщик")
		return
	default:
		a.writeError(w, http.StatusForbidden, "Директор заводит только менеджеров и официантов своего ресторана")
		return
	}

	var restaurant any // у поставщика ресторана нет
	if role != contract.RoleSupplier {
		restaurantID := auth.User.RestaurantID
		if owner {
			restaurantID = strings.TrimSpace(req.RestaurantID)
		}
		var exists int
		if a.DB.QueryRow("SELECT COUNT(*) FROM restaurants WHERE id=?", restaurantID).Scan(&exists); exists == 0 {
			a.writeError(w, http.StatusBadRequest, "Выберите ресторан сотрудника")
			return
		}
		restaurant = restaurantID
		if role == contract.RoleDirector {
			var directors int
			if err := a.DB.QueryRow("SELECT COUNT(*) FROM accounts WHERE role='director' AND restaurant_id=?", restaurantID).Scan(&directors); err != nil {
				a.writeError(w, http.StatusInternalServerError, "Не удалось проверить директора")
				return
			}
			if directors > 0 {
				a.writeError(w, http.StatusConflict, "У этого ресторана уже есть директор")
				return
			}
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать пароль")
		return
	}
	result, err := a.DB.Exec(
		"INSERT INTO accounts(name,email,password_hash,role,created_at,restaurant_id) VALUES(?,?,?,?,?,?)",
		login, login, string(hash), string(role), time.Now().UTC().Format(time.RFC3339Nano), restaurant,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			a.writeError(w, http.StatusConflict, "Пользователь с таким логином уже есть")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать сотрудника")
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать сотрудника")
		return
	}
	a.bumpRev()
	user, err := a.loadUser(id)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Сотрудник создан, но его не удалось прочитать")
		return
	}
	a.sendJSON(w, http.StatusCreated, user)
}

// DeleteManager удаляет аккаунт вместе с сессиями. Владелец удаляет любого, кроме себя;
// директор — менеджеров и официантов своего ресторана.
func (a *API) DeleteManager(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор пользователя")
		return
	}
	condition, args := "id=? AND role<>'owner'", []any{id}
	if auth.User.Role != contract.RoleOwner {
		condition, args = "id=? AND role IN ('manager','waiter') AND restaurant_id=?", []any{id, auth.User.RestaurantID}
	}
	var found int
	if err := a.DB.QueryRow("SELECT COUNT(*) FROM accounts WHERE "+condition, args...).Scan(&found); err != nil || found == 0 {
		a.writeError(w, http.StatusNotFound, "Сотрудник не найден")
		return
	}
	// Сессии удаляются явно, не полагаясь только на каскад внешнего ключа.
	a.forgetSessions(id)
	if _, err := a.DB.Exec("DELETE FROM auth_sessions WHERE user_id=?", id); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить пользователя")
		return
	}
	if _, err := a.DB.Exec("DELETE FROM accounts WHERE "+condition, args...); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить пользователя")
		return
	}
	a.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}

// maintainIfOwner — склад обслуживает только core; остальные сервисы лишь проверяют вход.
func (a *API) maintainIfOwner() error {
	if a.NoMaintenance {
		return nil
	}
	return a.maintain()
}

// RegisterNotificationRoutes — маршруты notification-service: лента уведомлений.
// Шлюз направляет сюда GET /api/alerts; уведомления приходят из Kafka (warehouse.events).
func (a *API) RegisterNotificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		a.sendJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "notification"})
	})
	mux.Handle("GET /api/alerts", a.protect(false, a.Alerts))
	a.Bus.Consume(events.TopicWarehouseEvents, "notification-service", a.storeWarehouseEvent)
}

// StartAIWorkers — ai-service: распознавание документов и аналитика Qwen из Kafka.
func (a *API) StartAIWorkers() {
	go requestimport.Warmup(requestimport.OllamaConfig())
	a.Bus.Consume(events.TopicDocumentJobs, "ai-service", a.processDocument)
	a.Bus.Consume(events.TopicAnalyticsRequests, "ai-service", a.processInsight)
}
