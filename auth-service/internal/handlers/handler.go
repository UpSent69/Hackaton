package handlers

import (
	"auth-service/internal/events"
	"database/sql"
	"encoding/json"
	"net/http"
)

// API contains the shared dependencies used by the HTTP handlers.
type API struct {
	DB *sql.DB
	KV  *KV         // Redis: сессии, кэш, ревизия, ограничение частоты (может быть недоступен)
	Bus *events.Bus // Kafka: распознавание документов, аналитика Qwen, события склада

	// NoMaintenance — сервис не обслуживает склад (ai-service, notification-service):
	// пересчёт статусов партий и стоп-листа на каждом запросе делает только core.
	NoMaintenance bool
}

var jwtSecret []byte

// SetJWTSecret installs the secret used to sign and verify API session tokens.
// The value is provided by the process environment rather than committed to source.
func SetJWTSecret(secret []byte) {
	jwtSecret = append([]byte(nil), secret...)
}

func (a *API) sendJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(data)
	}
}
