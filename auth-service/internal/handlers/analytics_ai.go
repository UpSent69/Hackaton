package handlers

import (
	"auth-service/internal/contract"
	"auth-service/internal/events"
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
	Season      string    `json:"season"`
	MenuPicks   []string  `json:"menuPicks"` // блюда из меню, которые выгодно продвигать по сезону
	NewDishes   []string  `json:"newDishes"` // сезонные блюда, которых в меню нет
	Model       string    `json:"model"`
	GeneratedAt time.Time `json:"generatedAt"`
	Cached      bool      `json:"cached"`
}

// Анализ модели живёт до следующего изменения данных: автообновление страницы
// не должно каждый раз запускать нейросеть.
//
// Сам анализ идёт в ai-service: core собирает статистику и публикует её в Kafka
// (analytics.requests), ai-service спрашивает Qwen и публикует ответ (analytics.results).
// После перерыва Ollama сначала загружает модель в память, и ответ может прийти позже, чем
// nginx держит запрос (60 с). Поэтому маршрут сразу отвечает 202 «считается», а страница
// переспрашивает с ?poll=1, пока не получит результат.
//
// Результат, признак «считается» и последняя ошибка хранятся в Redis (переживают перезапуск
// сервера, признак «считается» снимается сам через insightTimeout); без Redis — в памяти.
var insightMu sync.Mutex

// insightState — анализ одного ресторана в памяти (запасной путь без Redis).
type insightState struct {
	rev     int64
	cache   analyticsInsightResponse
	started time.Time // когда запущен анализ
	err     string
}

var insights = map[int64]*insightState{}

func insightFor(restaurantID int64) *insightState {
	state := insights[restaurantID]
	if state == nil {
		state = &insightState{rev: -1}
		insights[restaurantID] = state
	}
	return state
}

// insightKey — ключ Redis анализа ресторана: insight:{ресторан}:{result|running|error}.
func insightKey(restaurantID int64, name string) string {
	return "insight:" + strconv.FormatInt(restaurantID, 10) + ":" + name
}

// insightRequest — статистика для анализа (core → ai-service).
type insightRequest struct {
	RestaurantID int64     `json:"restaurantId"`
	Rev          int64     `json:"rev"`
	Facts        string    `json:"facts"`
	MenuNames    []string  `json:"menuNames"`
	Now          time.Time `json:"now"`
}

// insightResult — готовый анализ или понятная ошибка (ai-service → core).
type insightResult struct {
	RestaurantID int64                     `json:"restaurantId"`
	Rev          int64                     `json:"rev"`
	Result       *analyticsInsightResponse `json:"result,omitempty"`
	Error        string                    `json:"error,omitempty"`
}

const insightTimeout = 3 * time.Minute

type storedInsight struct {
	Rev    int64                    `json:"rev"`
	Result analyticsInsightResponse `json:"result"`
}

func (a *API) loadInsight(restaurantID int64) (analyticsInsightResponse, int64, bool) {
	if raw, ok := a.KV.Get(insightKey(restaurantID, "result")); ok {
		var stored storedInsight
		if json.Unmarshal([]byte(raw), &stored) == nil {
			return stored.Result, stored.Rev, true
		}
	}
	state := insightFor(restaurantID)
	return state.cache, state.rev, state.rev >= 0
}

func (a *API) saveInsight(restaurantID, rev int64, result analyticsInsightResponse) {
	state := insightFor(restaurantID)
	state.cache, state.rev = result, rev
	if encoded, err := json.Marshal(storedInsight{Rev: rev, Result: result}); err == nil {
		a.KV.Set(insightKey(restaurantID, "result"), string(encoded), 24*time.Hour)
	}
}

func (a *API) insightIsRunning(restaurantID int64) bool {
	if _, ok := a.KV.Get(insightKey(restaurantID, "running")); ok {
		return true
	}
	started := insightFor(restaurantID).started
	return !started.IsZero() && time.Since(started) < insightTimeout+30*time.Second
}

func (a *API) insightError(restaurantID int64) string {
	if message, ok := a.KV.Get(insightKey(restaurantID, "error")); ok {
		return message
	}
	return insightFor(restaurantID).err
}

func (a *API) setInsightError(restaurantID int64, message string) {
	insightFor(restaurantID).err = message
	if message == "" {
		a.KV.Del(insightKey(restaurantID, "error"))
	} else {
		a.KV.Set(insightKey(restaurantID, "error"), message, 10*time.Minute)
	}
}

// AnalyticsInsight передаёт локальной Qwen ту же статистику, что показывает вкладка «Аналитика».
// Без параметров — свежий анализ из памяти или запуск нового; ?refresh=1 — пересчитать
// принудительно; ?poll=1 — только узнать, готов ли запущенный анализ (новый не запускается).
func (a *API) AnalyticsInsight(w http.ResponseWriter, r *http.Request) {
	insightMu.Lock()
	defer insightMu.Unlock()
	pending := map[string]string{"status": "pending"}
	restaurantID := restaurantOf(r)
	cached, cachedRev, hasCached := a.loadInsight(restaurantID)

	if r.URL.Query().Get("poll") == "1" {
		switch message := a.insightError(restaurantID); {
		case a.insightIsRunning(restaurantID):
			a.sendJSON(w, http.StatusAccepted, pending)
		case message != "":
			a.writeError(w, http.StatusInternalServerError, message)
		case hasCached:
			a.sendJSON(w, http.StatusOK, cached)
		default:
			a.writeError(w, http.StatusConflict, "Анализ не запущен — нажмите «Обновить анализ»")
		}
		return
	}

	refresh := r.URL.Query().Get("refresh") == "1"
	requestMu.Lock()
	rev := a.currentRev()
	if !refresh && hasCached && rev == cachedRev {
		requestMu.Unlock()
		cached.Cached = true
		a.sendJSON(w, http.StatusOK, cached)
		return
	}
	if a.insightIsRunning(restaurantID) {
		requestMu.Unlock()
		a.sendJSON(w, http.StatusAccepted, pending)
		return
	}
	// Пересчёт по кнопке ограничен: каждый запуск занимает Qwen на десятки секунд.
	if refresh {
		if auth := currentAuth(r); auth != nil {
			if ok, wait := a.allow("insight:user:"+auth.User.ID, 6, time.Minute); !ok {
				requestMu.Unlock()
				a.writeError(w, http.StatusTooManyRequests, fmt.Sprintf("Анализ обновляли слишком часто. Попробуйте через %d с.", int(wait.Seconds())+1))
				return
			}
		}
	}
	facts, hasData, menuNames, err := a.analyticsFacts(r)
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

	// Замок в Redis не даёт запустить два анализа одновременно, даже из разных процессов.
	if acquired, ok := a.KV.SetNX(insightKey(restaurantID, "running"), "1", insightTimeout+30*time.Second); ok && !acquired {
		a.sendJSON(w, http.StatusAccepted, pending)
		return
	}
	request := insightRequest{RestaurantID: restaurantID, Rev: rev, Facts: facts, MenuNames: menuNames, Now: now}
	if err := a.Bus.Publish(context.Background(), events.TopicAnalyticsRequests, strconv.FormatInt(rev, 10), request); err != nil {
		log.Printf("Аналитика Qwen: Kafka недоступна: %v", err)
		a.KV.Del(insightKey(restaurantID, "running"))
		a.writeError(w, http.StatusServiceUnavailable, "Сервис аналитики временно недоступен, попробуйте через минуту")
		return
	}
	insightFor(restaurantID).started = time.Now()
	a.setInsightError(restaurantID, "")
	a.sendJSON(w, http.StatusAccepted, pending)
}

// processInsight (ai-service) спрашивает Qwen по статистике из analytics.requests
// и публикует ответ в analytics.results.
func (a *API) processInsight(value []byte) {
	var request insightRequest
	if err := json.Unmarshal(value, &request); err != nil {
		log.Printf("Аналитика Qwen: непонятный запрос: %v", err)
		return
	}
	baseURL, model := requestimport.OllamaConfig()
	ctx, cancel := context.WithTimeout(context.Background(), insightTimeout)
	defer cancel()
	started := time.Now()
	insight, err := requestimport.AnalyzeStats(ctx, request.Facts, baseURL, model)
	log.Printf("Аналитика Qwen: время=%s, ошибка=%v", time.Since(started), err)

	result := insightResult{RestaurantID: request.RestaurantID, Rev: request.Rev}
	if err != nil {
		result.Error = friendlyOllamaError(err, model)
	} else {
		menuPicks := onlyMenuDishes(insight.MenuPicks, request.MenuNames)
		log.Printf("Аналитика Qwen: блюд из меню предложено %d, совпало с меню %d", len(insight.MenuPicks), len(menuPicks))
		result.Result = &analyticsInsightResponse{
			Summary: insight.Summary, Risks: insight.Risks, Actions: insight.Actions,
			Season: insight.Season, MenuPicks: menuPicks, NewDishes: insight.NewDishes,
			Model: model, GeneratedAt: request.Now,
		}
	}
	if err := a.Bus.Publish(context.Background(), events.TopicAnalyticsResults, strconv.FormatInt(request.Rev, 10), result); err != nil {
		log.Printf("Аналитика Qwen: не удалось отправить ответ: %v", err)
	}
}

// storeInsightResult (core) сохраняет ответ из analytics.results и снимает признак «считается».
func (a *API) storeInsightResult(value []byte) {
	var result insightResult
	if err := json.Unmarshal(value, &result); err != nil {
		log.Printf("Аналитика Qwen: непонятный ответ: %v", err)
		return
	}
	insightMu.Lock()
	defer insightMu.Unlock()
	insightFor(result.RestaurantID).started = time.Time{}
	a.KV.Del(insightKey(result.RestaurantID, "running"))
	if result.Result == nil {
		a.setInsightError(result.RestaurantID, result.Error)
		return
	}
	a.saveInsight(result.RestaurantID, result.Rev, *result.Result)
}

// friendlyOllamaError объясняет частые причины на компьютере, где Ollama настроена иначе.
func friendlyOllamaError(err error, model string) string {
	text := err.Error()
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "no such host") || strings.Contains(lower, "connectex"):
		return "Qwen недоступна: Ollama не запущена или не принимает подключения из Docker. На компьютере с Ollama задайте OLLAMA_HOST=0.0.0.0:11434 и перезапустите её (см. REQUESTS_AI.md)."
	case strings.Contains(lower, "http 404") || (strings.Contains(lower, "model") && strings.Contains(lower, "not found")):
		return fmt.Sprintf("Модель %s не скачана в Ollama: выполните «ollama pull %s» и обновите анализ.", model, model)
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timeout"):
		return "Qwen не успела ответить за 3 минуты: компьютер с Ollama перегружен или слишком медленный. Попробуйте ещё раз."
	default:
		return "Qwen не ответила: " + text
	}
}

// analyticsFacts собирает статистику вкладки «Аналитика», склада и горячих предложений
// и записывает её простыми строками по-русски — маленькой модели так проще не ошибиться в числах.
func (a *API) analyticsFacts(r *http.Request) (string, bool, []string, error) {
	var analytics contract.Analytics
	if err := a.captureJSON(a.Analytics, r, &analytics); err != nil {
		return "", false, nil, err
	}
	var stock []contract.StockItem
	if err := a.captureJSON(a.Stock, r, &stock); err != nil {
		return "", false, nil, err
	}
	var menu []contract.Dish
	if err := a.captureJSON(a.Menu, r, &menu); err != nil {
		return "", false, nil, err
	}
	now, _ := a.nowAndSpeed()
	hot, err := a.computeHotOffers(restaurantOf(r), now)
	if err != nil {
		return "", false, nil, err
	}
	sales, err := a.dishSales(restaurantOf(r), now)
	if err != nil {
		return "", false, nil, err
	}
	hasData := len(stock) > 0 || analytics.Orders.Accepted+analytics.Orders.Rejected > 0

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	hours := func(at time.Time) string { return formatInsightHours(at.Sub(now).Hours()) }
	limit := func(n int) int { return int(math.Min(float64(n), 10)) }

	local := now.In(moscow())
	line("Сегодня %s, сезон: %s.%s", local.Format("02.01.2006"), seasonName(local), upcomingHolidays(local))
	line("Сезонные блюда, которые в это время года заказывают чаще: %s.", seasonalDishes(local))
	line("Заказы: оформлено %d, отклонено из-за нехватки продуктов %d.", analytics.Orders.Accepted, analytics.Orders.Rejected)

	line("\nМеню (порций продано: всего / в этом сезоне / за 7 дней; сколько можно приготовить из остатков):")
	menuNames := make([]string, 0, len(menu))
	if len(menu) == 0 {
		line("- меню пустое")
	}
	for _, dish := range menu {
		menuNames = append(menuNames, dish.Name)
		parts := make([]string, 0, len(dish.Ingredients))
		for _, ingredient := range dish.Ingredients {
			parts = append(parts, ingredient.Name)
		}
		sold := sales[dish.DishID]
		state := fmt.Sprintf("можно приготовить %d порц.", dish.AvailablePortions)
		if dish.Stopped {
			state = "в стоп-листе"
		}
		line("- %s (%s): продано %d / %d / %d; %s", dish.Name, strings.Join(parts, ", "), sold.Total, sold.Season, sold.Week, state)
	}

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
	return b.String(), hasData, menuNames, nil
}

type dishSold struct{ Total, Season, Week int64 }

// dishSales — сколько порций каждого блюда продано: всего, в текущем сезоне (те же месяцы любого года)
// и за последние 7 дней по часам приложения.
func (a *API) dishSales(restaurantID int64, now time.Time) (map[string]dishSold, error) {
	rows, err := a.DB.Query("SELECT oi.dish_id,oi.portions,o.created_at FROM order_items oi JOIN orders o ON o.id=oi.order_id WHERE o.status='accepted' AND o.restaurant_id=?", restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	season := seasonName(now.In(moscow()))
	result := make(map[string]dishSold)
	for rows.Next() {
		var dishID, created string
		var portions int64
		if err := rows.Scan(&dishID, &portions, &created); err != nil {
			return nil, err
		}
		at := parseTime(created)
		item := result[dishID]
		item.Total += portions
		if seasonName(at.In(moscow())) == season {
			item.Season += portions
		}
		if !at.After(now) && now.Sub(at) <= 7*24*time.Hour {
			item.Week += portions
		}
		result[dishID] = item
	}
	return result, rows.Err()
}

func moscow() *time.Location {
	if location, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return location
	}
	return time.FixedZone("MSK", 3*60*60)
}

func seasonName(at time.Time) string {
	switch at.Month() {
	case time.December, time.January, time.February:
		return "зима"
	case time.March, time.April, time.May:
		return "весна"
	case time.June, time.July, time.August:
		return "лето"
	default:
		return "осень"
	}
}

// seasonalDishes — подсказка модели: что в России чаще заказывают в этот сезон и перед праздниками.
// Сервер даёт список только для текущего сезона, чтобы модель не советовала окрошку осенью.
func seasonalDishes(at time.Time) string {
	dishes := map[string]string{
		"зима":  "оливье, селёдка под шубой, холодец, борщ, пельмени, глинтвейн, горячий шоколад, блюда с мандаринами",
		"весна": "блины (Масленица), кулич и творожная пасха (Пасха), салаты с редисом и молодой зеленью, щавелевый суп, молодой картофель",
		"лето":  "окрошка, свекольник и холодник, летние салаты из свежих овощей, шашлык, лимонады и холодный чай, мороженое и ягодные десерты",
		"осень": "тыквенный суп и тыквенные десерты, грибной суп и жаркое с грибами, пироги с яблоками, рагу из овощей нового урожая, глинтвейн, кофе и какао со специями",
	}
	list := dishes[seasonName(at)]
	if strings.Contains(upcomingHolidays(at), "Новый год") || strings.Contains(upcomingHolidays(at), "канун Нового года") {
		list += "; к Новому году — оливье, мясная и сырная нарезка, салаты-закуски, мандарины, шампанское"
	}
	return list
}

// upcomingHolidays — праздники, до которых меньше 30 дней: под них стоит готовить меню заранее.
func upcomingHolidays(at time.Time) string {
	holidays := []struct {
		month time.Month
		day   int
		name  string
	}{
		{time.January, 1, "Новый год"}, {time.January, 7, "Рождество"}, {time.February, 14, "День святого Валентина"},
		{time.February, 23, "23 февраля"}, {time.March, 8, "8 марта"}, {time.May, 1, "1 мая"}, {time.May, 9, "День Победы"},
		{time.June, 12, "День России"}, {time.September, 1, "1 сентября"}, {time.November, 4, "День народного единства"},
		{time.December, 31, "канун Нового года"},
	}
	today := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	found := make([]string, 0)
	for _, holiday := range holidays {
		date := time.Date(at.Year(), holiday.month, holiday.day, 0, 0, 0, 0, at.Location())
		if date.Before(today) {
			date = date.AddDate(1, 0, 0)
		}
		if days := int(date.Sub(today).Hours() / 24); days <= 30 {
			found = append(found, fmt.Sprintf("%s через %d дн.", holiday.name, days))
		}
	}
	if len(found) == 0 {
		return ""
	}
	return " Ближайшие праздники: " + strings.Join(found, ", ") + "."
}

// onlyMenuDishes оставляет рекомендации, в которых названо блюдо из меню: маленькая модель
// иногда советует «из меню» то, чего в нём нет.
func onlyMenuDishes(picks, menuNames []string) []string {
	result := make([]string, 0, len(picks))
	for _, pick := range picks {
		normalized := strings.ToLower(pick)
		for _, name := range menuNames {
			if strings.Contains(normalized, strings.ToLower(name)) {
				result = append(result, pick)
				break
			}
		}
	}
	return result
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
