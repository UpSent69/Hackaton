package handlers

import (
	"auth-service/internal/contract"
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
}

// The warehouse demo is intentionally serialized. SQLite has one connection and this
// also ensures that two orders cannot both spend the same final batch quantity.
var requestMu sync.Mutex

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		a.sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/auth/register", a.Register)
	mux.HandleFunc("POST /api/auth/login", a.Login)
	mux.HandleFunc("GET /api/auth/registration", a.Registration)

	authed := func(method, path string, admin bool, handler http.HandlerFunc) {
		mux.Handle(method+" "+path, a.protect(admin, handler))
	}

	authed("POST", "/api/auth/logout", false, a.Logout)
	authed("GET", "/api/auth/me", false, a.Me)
	authed("GET", "/api/users", true, a.Users)
	authed("POST", "/api/users/{id}/role", true, a.SetUserRole)
	authed("POST", "/api/users", true, a.CreateManager)
	authed("DELETE", "/api/users/{id}", true, a.DeleteManager)

	authed("GET", "/api/products", false, a.Products)
	authed("POST", "/api/products/{productId}/price", true, a.UpdateProductPrice)
	authed("GET", "/api/requests", false, a.Requests)
	authed("GET", "/api/requests/{id}", false, a.RequestByID)
	authed("POST", "/api/requests", true, a.CreateRequest)
	mux.Handle("POST /api/requests/parse", a.protectLongRunning(false, a.ParseRequestFile))
	authed("GET", "/api/stock", false, a.Stock)
	authed("GET", "/api/stock/{productId}/batches", false, a.Batches)
	authed("GET", "/api/expiry", false, a.Expiry)
	authed("GET", "/api/supplies", false, a.Supplies)
	authed("POST", "/api/supplies", true, a.CreateSupply)
	mux.Handle("POST /api/supplies/parse", a.protectLongRunning(true, a.ParseSupplyFile))
	authed("DELETE", "/api/supplies/{id}", true, a.DeleteSupply)
	authed("DELETE", "/api/supplies/{id}/items/{productId}", true, a.DeleteSupplyItem)

	authed("GET", "/api/menu", false, a.Menu)
	authed("POST", "/api/dishes", true, a.CreateDish)
	authed("DELETE", "/api/dishes/{id}", true, a.DeleteDish)
	authed("POST", "/api/orders/validate", false, a.ValidateOrder)
	authed("POST", "/api/orders", true, a.CreateOrder)
	authed("GET", "/api/orders", false, a.Orders)

	authed("GET", "/api/alerts", false, a.Alerts)
	authed("GET", "/api/pulse", false, a.Pulse)
	authed("GET", "/api/hot-offers", false, a.HotOffers)
	authed("GET", "/api/analytics", false, a.Analytics)
	mux.Handle("GET /api/analytics/ai", a.protectLongRunning(false, a.AnalyticsInsight))

	authed("GET", "/api/demo/state", false, a.DemoState)
	authed("POST", "/api/demo/clock", true, a.DemoClock)
	authed("POST", "/api/demo/advance", true, a.DemoAdvance)
	authed("POST", "/api/demo/seed", true, a.DemoSeed)
	authed("POST", "/api/demo/restock", true, a.DemoRestock)
	authed("POST", "/api/demo/reset", true, a.DemoReset)
}

func (a *API) protect(adminOnly bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()

		auth, err := a.authenticate(r)
		if err != nil {
			a.writeError(w, http.StatusUnauthorized, "Требуется вход или срок сессии истёк")
			return
		}
		if adminOnly && auth.User.Role != contract.RoleAdmin {
			a.writeError(w, http.StatusForbidden, "Недостаточно прав для выполнения операции")
			return
		}
		if err := a.maintain(); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось обновить состояние склада")
			return
		}
		ctx := context.WithValue(r.Context(), authContextKey, auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// protectLongRunning authenticates under the SQLite request lock, then releases it before
// CPU/network-heavy work such as AI extraction. The handler only reads the catalogue afterwards.
func (a *API) protectLongRunning(adminOnly bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		auth, err := a.authenticate(r)
		if err != nil {
			requestMu.Unlock()
			a.writeError(w, http.StatusUnauthorized, "Требуется вход или срок сессии истёк")
			return
		}
		if adminOnly && auth.User.Role != contract.RoleAdmin {
			requestMu.Unlock()
			a.writeError(w, http.StatusForbidden, "Недостаточно прав для выполнения операции")
			return
		}
		if err := a.maintain(); err != nil {
			requestMu.Unlock()
			a.writeError(w, http.StatusInternalServerError, "Не удалось обновить состояние склада")
			return
		}
		ctx := context.WithValue(r.Context(), authContextKey, auth)
		requestMu.Unlock()
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

	var id int64
	var name, email, role, expiresAt string
	var revoked int
	err = a.DB.QueryRow(
		"SELECT a.id, a.name, a.email, a.role, s.expires_at, s.revoked "+
			"FROM accounts a JOIN auth_sessions s ON s.user_id = a.id "+
			"WHERE a.id = ? AND s.jti = ?",
		int64(idValue), tokenID,
	).Scan(&id, &name, &email, &role, &expiresAt, &revoked)
	if err != nil || revoked != 0 {
		return nil, errors.New("session not found")
	}
	expiration, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !time.Now().UTC().Before(expiration) {
		return nil, errors.New("session expired")
	}
	if role != string(contract.RoleAdmin) && role != string(contract.RoleManager) {
		return nil, errors.New("invalid role")
	}
	return &authContext{
		User: contract.User{ID: strconv.FormatInt(id, 10), Name: name, Email: email, Role: contract.Role(role)},
		TokenID: tokenID,
	}, nil
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
	// Самостоятельно регистрируются только первые два администратора.
	// Менеджеров заводит администратор на странице «Администрирование».
	open, err := a.registrationOpen()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить пользователей")
		return
	}
	if !open {
		a.writeError(w, http.StatusForbidden, "Регистрация закрыта: аккаунт менеджера выдаёт администратор")
		return
	}
	role := contract.RoleAdmin
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
	user := contract.User{ID: strconv.FormatInt(id, 10), Name: name, Email: storedEmail, Role: contract.Role(role)}
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
	return contract.Session{Token: tokenString, User: user}, nil
}

func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	if auth != nil {
		_, _ = a.DB.Exec("UPDATE auth_sessions SET revoked=1 WHERE jti=?", auth.TokenID)
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

func (a *API) Users(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query("SELECT id,name,email,role FROM accounts ORDER BY name COLLATE NOCASE,email COLLATE NOCASE")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить пользователей")
		return
	}
	defer rows.Close()
	users := make([]contract.User, 0)
	for rows.Next() {
		var id int64
		var user contract.User
		var role string
		if err := rows.Scan(&id, &user.Name, &user.Email, &role); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать пользователей")
			return
		}
		user.ID = strconv.FormatInt(id, 10)
		user.Role = contract.Role(role)
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать пользователей")
		return
	}
	a.sendJSON(w, http.StatusOK, users)
}

func (a *API) SetUserRole(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if auth == nil || id == auth.User.ID {
		a.writeError(w, http.StatusBadRequest, "Нельзя менять собственную роль")
		return
	}
	var req contract.SetRoleRequest
	if err := decodeJSON(w, r, &req); err != nil || (req.Role != contract.RoleAdmin && req.Role != contract.RoleManager) {
		a.writeError(w, http.StatusBadRequest, "Выберите роль «admin» или «manager»")
		return
	}
	result, err := a.DB.Exec("UPDATE accounts SET role=? WHERE id=?", string(req.Role), id)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось изменить роль")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		a.writeError(w, http.StatusNotFound, "Пользователь не найден")
		return
	}
	a.bumpRev()
	var uid int64
	var user contract.User
	var role string
	if err := a.DB.QueryRow("SELECT id,name,email,role FROM accounts WHERE id=?", id).Scan(&uid, &user.Name, &user.Email, &role); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать пользователя")
		return
	}
	user.ID = strconv.FormatInt(uid, 10)
	user.Role = contract.Role(role)
	a.sendJSON(w, http.StatusOK, user)
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

func (a *API) currentRev() int64 {
	var value string
	if err := a.DB.QueryRow("SELECT value FROM app_meta WHERE key='revision'").Scan(&value); err != nil {
		return 0
	}
	rev, _ := strconv.ParseInt(value, 10, 64)
	return rev
}

func (a *API) bumpRev() {
	_, _ = a.DB.Exec("UPDATE app_meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")
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

// registrationOpen — регистрация открыта, пока в системе меньше двух администраторов.
func (a *API) registrationOpen() (bool, error) {
	var admins int
	err := a.DB.QueryRow("SELECT COUNT(*) FROM accounts WHERE role=?", string(contract.RoleAdmin)).Scan(&admins)
	return admins < 2, err
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

// CreateManager — администратор заводит менеджера по логину и паролю. Логин хранится
// в том же поле, что и почта администратора, поэтому входит менеджер через ту же форму.
func (a *API) CreateManager(w http.ResponseWriter, r *http.Request) {
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
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать пароль")
		return
	}
	result, err := a.DB.Exec(
		"INSERT INTO accounts(name,email,password_hash,role,created_at) VALUES(?,?,?,?,?)",
		login, login, string(hash), string(contract.RoleManager), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			a.writeError(w, http.StatusConflict, "Пользователь с таким логином уже есть")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать менеджера")
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать менеджера")
		return
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusCreated, contract.User{ID: strconv.FormatInt(id, 10), Name: login, Email: login, Role: contract.RoleManager})
}

// DeleteManager удаляет менеджера вместе с его сессиями. Администраторов так удалить нельзя.
func (a *API) DeleteManager(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор пользователя")
		return
	}
	// Сессии удаляются явно, не полагаясь только на каскад внешнего ключа.
	if _, err := a.DB.Exec("DELETE FROM auth_sessions WHERE user_id IN (SELECT id FROM accounts WHERE id=? AND role=?)", id, string(contract.RoleManager)); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить пользователя")
		return
	}
	result, err := a.DB.Exec("DELETE FROM accounts WHERE id=? AND role=?", id, string(contract.RoleManager))
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить пользователя")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		a.writeError(w, http.StatusNotFound, "Менеджер не найден")
		return
	}
	a.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}
