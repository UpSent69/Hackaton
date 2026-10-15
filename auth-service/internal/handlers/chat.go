package handlers

import (
	"auth-service/internal/contract"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Общий чат сети: пишут сотрудники всех трёх ресторанов, владелец и поставщик. Имя, должность
// и ресторан автора сохраняются в сообщении на момент отправки — после удаления аккаунта
// переписка остаётся читаемой.

const (
	chatHistory   = 200  // сколько последних сообщений отдаёт GET /api/chat
	chatMaxLength = 1000 // символов в сообщении
	chatNetwork   = "Вся сеть"
)

// ChatMessages — последние сообщения (новые в конце). ?after=ID — только сообщения новее ID
// (для счётчика непрочитанных, пока панель чата закрыта).
func (a *API) ChatMessages(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	query := "SELECT id,user_id,author_name,role,restaurant_name,text,created_at FROM " +
		"(SELECT * FROM chat_messages ORDER BY id DESC LIMIT ?) ORDER BY id"
	args := []any{chatHistory}
	if after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil && after > 0 {
		query = "SELECT id,user_id,author_name,role,restaurant_name,text,created_at FROM chat_messages WHERE id>? ORDER BY id LIMIT ?"
		args = []any{after, chatHistory}
	}
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить чат")
		return
	}
	defer rows.Close()
	messages := make([]contract.ChatMessage, 0)
	for rows.Next() {
		var message contract.ChatMessage
		var id, userID int64
		var created string
		if err := rows.Scan(&id, &userID, &message.Author, &message.Role, &message.Restaurant, &message.Text, &created); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать чат")
			return
		}
		message.ID = strconv.FormatInt(id, 10)
		message.At = parseTime(created)
		message.Mine = strconv.FormatInt(userID, 10) == auth.User.ID
		message.CanDelete = message.Mine || auth.User.Role == contract.RoleOwner
		messages = append(messages, message)
	}
	a.sendJSON(w, http.StatusOK, messages)
}

// SendChatMessage — новое сообщение в общий чат.
func (a *API) SendChatMessage(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Напишите сообщение")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len([]rune(text)) > chatMaxLength {
		a.writeError(w, http.StatusBadRequest, "Сообщение — от 1 до 1000 символов")
		return
	}
	if ok, wait := a.allow("chat:user:"+auth.User.ID, 30, time.Minute); !ok {
		a.writeError(w, http.StatusTooManyRequests, "Слишком много сообщений подряд. Подождите "+strconv.Itoa(int(wait.Seconds())+1)+" с.")
		return
	}
	restaurant := auth.User.RestaurantName
	if restaurant == "" {
		restaurant = chatNetwork
	}
	if _, err := a.DB.Exec(
		"INSERT INTO chat_messages(user_id,author_name,role,restaurant_name,text,created_at) VALUES(?,?,?,?,?,?)",
		auth.User.ID, auth.User.Name, string(auth.User.Role), restaurant, text, formatTime(time.Now().UTC()),
	); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось отправить сообщение")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteChatMessage — автор удаляет своё сообщение, владелец — любое.
func (a *API) DeleteChatMessage(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный номер сообщения")
		return
	}
	condition, args := "id=? AND user_id=?", []any{id, auth.User.ID}
	if auth.User.Role == contract.RoleOwner {
		condition, args = "id=?", []any{id}
	}
	result, err := a.DB.Exec("DELETE FROM chat_messages WHERE "+condition, args...)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить сообщение")
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		a.writeError(w, http.StatusNotFound, "Сообщение не найдено или его нельзя удалить")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
