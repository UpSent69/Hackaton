package main

import (
	"auth-service/internal/database"
	"auth-service/internal/events"
	"auth-service/internal/handlers"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	dbPath := os.Getenv("SQLITE_DB_PATH")
	if dbPath == "" {
		dbPath = "./data/app.db"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		log.Fatalf("Не удалось создать директорию для БД: %v", err)
	}

	// Один бинарник — три микросервиса, роль задаёт SERVICE:
	//   core         — HTTP API сайта (вход, склад, поставки, меню, заказы, заявки, аналитика);
	//   ai           — распознавание документов и аналитика Qwen (задачи из Kafka);
	//   notification — уведомления из событий склада (Kafka) и GET /api/alerts.
	// Все сервисы работают с общей базой SQLite и общим Redis.
	service := strings.TrimSpace(os.Getenv("SERVICE"))
	if service == "" {
		service = "core"
	}
	open := database.Open
	if service == "core" {
		open = database.Init
	}
	db, err := open(dbPath)
	if err != nil {
		log.Fatalf("Ошибка инициализации БД: %v", err)
	}
	defer db.Close()

	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if secret == "" {
		secret = "local-development-only-change-this-secret"
		log.Println("ВНИМАНИЕ: JWT_SECRET не задан; используется ключ для локальной разработки.")
	}
	if len(secret) < 32 {
		log.Fatal("JWT_SECRET должен содержать не менее 32 символов")
	}
	handlers.SetJWTSecret([]byte(secret))

	api := &handlers.API{
		DB: db, KV: handlers.NewKV(os.Getenv("REDIS_ADDR")), Bus: events.New(os.Getenv("KAFKA_BROKERS")),
		NoMaintenance: service != "core",
	}
	mux := http.NewServeMux()
	switch service {
	case "core":
		api.RegisterRoutes(mux)
	case "ai":
		api.StartAIWorkers()
		mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","service":"ai"}`))
		})
	case "notification":
		api.RegisterNotificationRoutes(mux)
	default:
		log.Fatalf("Неизвестный SERVICE=%q: ожидается core, ai или notification", service)
	}

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}
	handler := api.RevisionMiddleware(corsMiddleware(mux))
	log.Printf("Stoplist %s запущен на порту %s", service, port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
