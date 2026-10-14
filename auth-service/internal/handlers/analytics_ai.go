package handlers

import (
	"auth-service/internal/contract"
	"auth-service/internal/requestimport"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type analyticsInsightResponse struct {
	Summary     string    `json:"summary"`
	Risks       []string  `json:"risks"`
	Actions     []string  `json:"actions"`
	Model       string    `json:"model"`
	GeneratedAt time.Time `json:"generatedAt"`
	Cached      bool      `json:"cached"`
}

// Анализ модели живёт до следующего изменения данных: автообновление страницы
// не должно каждый раз запускать нейросеть. Генерации идут по одной.
var (
	insightMu    sync.Mutex
	insightRev   int64 = -1
	insightCache analyticsInsightResponse
)

// AnalyticsInsight передаёт локальной Qwen ту же статистику, что показывает вкладка
// «Аналитика», и возвращает её выводы. ?refresh=1 — пересчитать, даже если данные не менялись.
func (a *API) AnalyticsInsight(w http.ResponseWriter, r *http.Request) {
	insightMu.Lock()
	defer insightMu.Unlock()

	refresh := r.URL.Query().Get("refresh") == "1"
	requestMu.Lock()
	rev := a.currentRev()
	if !refresh && rev == insightRev {
		requestMu.Unlock()
		cached := insightCache
		cached.Cached = true
		a.sendJSON(w, http.StatusOK, cached)
		return
	}
	facts, hasData, err := a.analyticsFacts(r)
	now, _ := a.nowAndSpeed()
	requestMu.Unlock()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось собрать статистику для анализа")
		return
	}
	if !hasData {
		a.writeError(w, http.StatusUnprocessableEntity, "Данных для анализа пока нет: примите поставку и оформите несколько заказов.")
		return
	}

	baseURL, model := requestimport.OllamaConfig()
	ctx, cancel := context.WithTimeout(r.Context(), 52*time.Second)
	defer cancel()
	started := time.Now()
	insight, err := requestimport.AnalyzeStats(ctx, facts, baseURL, model)
	log.Printf("Аналитика Qwen: время=%s, ошибка=%v", time.Since(started), err)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Qwen не ответила: "+err.Error())
		return
	}

	insightCache = analyticsInsightResponse{
		Summary: insight.Summary, Risks: insight.Risks, Actions: insight.Actions,
		Model: model, GeneratedAt: now,
	}
	insightRev = rev
	a.sendJSON(w, http.StatusOK, insightCache)
}

// analyticsFacts собирает статистику вкладки «Аналитика», склада и горячих предложений
// и записывает её простыми строками по-русски — маленькой модели так проще не ошибиться в числах.
func (a *API) analyticsFacts(r *http.Request) (string, bool, error) {
	var analytics contract.Analytics
	if err := a.captureJSON(a.Analytics, r, &analytics); err != nil {
		return "", false, err
	}
	var stock []contract.StockItem
	if err := a.captureJSON(a.Stock, r, &stock); err != nil {
		return "", false, err
	}
	now, _ := a.nowAndSpeed()
	hot, err := a.computeHotOffers(now)
	if err != nil {
		return "", false, err
	}
	hasData := len(stock) > 0 || analytics.Orders.Accepted+analytics.Orders.Rejected > 0

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	hours := func(at time.Time) string { return formatInsightHours(at.Sub(now).Hours()) }
	limit := func(n int) int { return int(math.Min(float64(n), 10)) }

	line("Заказы: оформлено %d, отклонено из-за нехватки продуктов %d.", analytics.Orders.Accepted, analytics.Orders.Rejected)

	line("\nОстатки на складе:")
	if len(stock) == 0 {
		line("- склад пуст")
	}
	statusName := map[contract.ExpiryStatus]string{
		contract.StatusOK: "в норме", contract.StatusWarning: "скоро истекает",
		contract.StatusCritical: "срок почти вышел", contract.StatusExpired: "просрочено",
	}
	for _, item := range stock {
		text := fmt.Sprintf("- %s: доступно %s, %s", item.Name, formatInsightQty(item.Available, item.Unit), statusName[item.Status])
		if item.NearestExpiresAt != nil {
			text += ", ближайший срок через " + hours(*item.NearestExpiresAt)
		}
		if item.Expired > 0 {
			text += ", просрочено и не списано " + formatInsightQty(item.Expired, item.Unit)
		}
		line("%s", text)
	}

	line("\nГорячие предложения (блюда, которые стоит продвигать, чтобы продукт не испортился):")
	if len(hot) == 0 {
		line("- нет")
	}
	for _, offer := range hot[:limit(len(hot))] {
		reason := offer.Reasons[0]
		line("- %s (приоритет %d из 100): %s, под риском %s, истекает через %s; продать %d порц.",
			offer.Name, offer.Score, reason.ProductName, formatInsightQty(reason.AtRisk, reason.Unit), hours(reason.ExpiresAt), offer.Target.Portions)
	}

	line("\nПрогноз стоп-листа:")
	if len(analytics.Forecast) == 0 {
		line("- блюд в меню нет")
	}
	for _, item := range analytics.Forecast[:limit(len(analytics.Forecast))] {
		switch {
		case item.Stopped:
			line("- %s: уже в стоп-листе, не хватает: %s", item.Name, item.ProductName)
		case item.Cause == "consumption":
			line("- %s: уйдёт в стоп через %s — при текущих продажах закончится %s", item.Name, formatInsightHours(item.Hours), item.ProductName)
		default:
			line("- %s: уйдёт в стоп через %s — истечёт срок годности: %s", item.Name, formatInsightHours(item.Hours), item.ProductName)
		}
	}

	line("\nЧто пора заказать:")
	if len(analytics.Purchases) == 0 {
		line("- ничего, запасов хватает больше чем на двое суток")
	}
	for _, item := range analytics.Purchases[:limit(len(analytics.Purchases))] {
		if item.HoursLeft != nil {
			line("- %s: остаток %s, хватит на %s, расход в сутки %s, заказать %s",
				item.Name, formatInsightQty(item.Usable, item.Unit), formatInsightHours(*item.HoursLeft),
				formatInsightQty(int64(math.Round(item.PerDay)), item.Unit), formatInsightQty(item.Quantity, item.Unit))
		} else {
			line("- %s: остаток %s — меньше чем на 5 порций, продаж пока не было, заказать %s",
				item.Name, formatInsightQty(item.Usable, item.Unit), formatInsightQty(item.Quantity, item.Unit))
		}
	}

	line("\nСписания по сроку годности:")
	if len(analytics.Writeoffs) == 0 {
		line("- не было")
	}
	for _, item := range analytics.Writeoffs[:limit(len(analytics.Writeoffs))] {
		line("- %s: списано %s из поступивших %s (%d%%)", item.Name, formatInsightQty(item.Quantity, item.Unit),
			formatInsightQty(item.Received, item.Unit), int(math.Round(item.Share*100)))
	}

	line("\nДефицит (отказы в заказах из-за нехватки):")
	if len(analytics.Deficits) == 0 {
		line("- не было")
	}
	for _, item := range analytics.Deficits[:limit(len(analytics.Deficits))] {
		line("- %s: не хватило %s, отказов %d", item.Name, formatInsightQty(item.Missing, item.Unit), item.Rejections)
	}
	return b.String(), hasData, nil
}

// captureJSON вызывает обработчик API без сети и читает его JSON-ответ.
func (a *API) captureJSON(handler http.HandlerFunc, r *http.Request, target any) error {
	recorder := httptest.NewRecorder()
	handler(recorder, r)
	if recorder.Code != http.StatusOK {
		return fmt.Errorf("handler returned HTTP %d", recorder.Code)
	}
	return json.Unmarshal(recorder.Body.Bytes(), target)
}

func formatInsightQty(quantity int64, unit contract.Unit) string {
	switch unit {
	case contract.UnitGram:
		if quantity >= 1000 {
			return trimDecimal(float64(quantity)/1000) + " кг"
		}
		return strconv.FormatInt(quantity, 10) + " г"
	case contract.UnitMilliliter:
		if quantity >= 1000 {
			return trimDecimal(float64(quantity)/1000) + " л"
		}
		return strconv.FormatInt(quantity, 10) + " мл"
	default:
		return strconv.FormatInt(quantity, 10) + " шт"
	}
}

func formatInsightHours(hours float64) string {
	if hours < 0 {
		hours = 0
	}
	if hours >= 48 {
		return trimDecimal(hours/24) + " сут"
	}
	return strconv.Itoa(int(math.Round(hours))) + " ч"
}

func trimDecimal(value float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), "0"), ".")
}
