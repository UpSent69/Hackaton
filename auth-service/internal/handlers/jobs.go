package handlers

import (
	"auth-service/internal/events"
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
// не дольше 60 секунд, поэтому core сразу отвечает 202 с номером задачи и публикует документ
// в Kafka (documents.parse). ai-service распознаёт его и публикует результат
// (documents.results), core сохраняет результат в Redis (без Redis — в памяти), а страница
// забирает его через GET /api/jobs/{id}.

const (
	jobTTL     = time.Hour
	jobTimeout = 3 * time.Minute
)

type documentJob struct {
	ID           string `json:"id"`
	RestaurantID int64  `json:"restaurantId"` // каталог и меню какого ресторана сопоставлять
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

type documentResult struct {
	JobID string `json:"jobId"`
	jobResult
}

// Запасное хранилище задач core, пока Redis недоступен.
var (
	memoryJobsMu sync.Mutex
	memoryJobs   = make(map[string]*memoryJob)
)

type memoryJob struct {
	result *jobResult
	at     time.Time
}

func newJobID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// enqueueDocument публикует документ в Kafka и отвечает 202 с номером задачи.
func (a *API) enqueueDocument(w http.ResponseWriter, restaurantID int64, kind, filename, hash string, source requestimport.Source) {
	job := documentJob{ID: newJobID(), RestaurantID: restaurantID, Kind: kind, Filename: filename, Hash: hash, Text: source.Text, Image: source.Image, MIME: source.ImageMIME}
	if err := a.Bus.Publish(context.Background(), events.TopicDocumentJobs, job.ID, job); err != nil {
		log.Printf("Очередь документов: Kafka недоступна: %v", err)
		a.writeError(w, http.StatusServiceUnavailable, "Сервис распознавания временно недоступен, попробуйте через минуту")
		return
	}
	// Отметка «задача есть» — чтобы GET /api/jobs/{id} отвечал 202, а не 404.
	if !a.KV.Set("job:"+job.ID, "pending", jobTTL) {
		memoryJobsMu.Lock()
		memoryJobs[job.ID] = &memoryJob{at: time.Now()}
		memoryJobsMu.Unlock()
	}
	log.Printf("Очередь документов: задача %s (%s, %s) отправлена в Kafka", job.ID, kind, filename)
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

	if result == nil {
		if raw, ok := a.KV.Get("job:" + id + ":result"); ok {
			result = &jobResult{}
			if json.Unmarshal([]byte(raw), result) != nil {
				result = nil
			}
		} else if _, ok := a.KV.Get("job:" + id); !ok && entry == nil {
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

// storeDocumentResult (core) сохраняет результат из documents.results для GET /api/jobs/{id}.
func (a *API) storeDocumentResult(value []byte) {
	var message documentResult
	if err := json.Unmarshal(value, &message); err != nil || message.JobID == "" {
		log.Printf("Очередь документов: непонятный результат: %v", err)
		return
	}
	result := message.jobResult
	encoded, _ := json.Marshal(result)
	memoryJobsMu.Lock()
	defer memoryJobsMu.Unlock()
	if entry := memoryJobs[message.JobID]; entry != nil || !a.KV.Set("job:"+message.JobID+":result", string(encoded), jobTTL) {
		memoryJobs[message.JobID] = &memoryJob{result: &result, at: time.Now()}
	}
	log.Printf("Очередь документов: результат задачи %s получен, HTTP %d", message.JobID, result.Status)
}

// processDocument (ai-service) распознаёт документ из documents.parse и публикует результат.
// Обработчик один: Ollama всё равно отвечает по очереди.
func (a *API) processDocument(value []byte) {
	var job documentJob
	if err := json.Unmarshal(value, &job); err != nil || job.ID == "" {
		log.Printf("Распознавание: непонятная задача: %v", err)
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	recorder := httptest.NewRecorder()
	source := requestimport.Source{Text: job.Text, Image: job.Image, ImageMIME: job.MIME}
	if job.Kind == "supply" {
		a.finishSupplyParse(ctx, recorder, job.RestaurantID, job.Filename, job.Hash, source)
	} else {
		a.finishRequestParse(ctx, recorder, job.RestaurantID, job.Filename, job.Hash, source)
	}
	result := documentResult{JobID: job.ID, jobResult: jobResult{Status: recorder.Code, Body: json.RawMessage(recorder.Body.Bytes())}}
	log.Printf("Распознавание: задача %s (%s) готова за %s, HTTP %d", job.ID, job.Kind, time.Since(started).Round(time.Second), result.Status)
	if err := a.Bus.Publish(context.Background(), events.TopicDocumentResults, job.ID, result); err != nil {
		log.Printf("Распознавание: не удалось отправить результат задачи %s: %v", job.ID, err)
	}
}

// cleanupMemoryJobs удаляет из памяти core устаревшие задачи.
func cleanupMemoryJobs() {
	for range time.Tick(10 * time.Minute) {
		memoryJobsMu.Lock()
		for id, entry := range memoryJobs {
			if time.Since(entry.at) > jobTTL {
				delete(memoryJobs, id)
			}
		}
		memoryJobsMu.Unlock()
	}
}
