package handlers

import (
	"auth-service/internal/contract"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *API) Alerts(w http.ResponseWriter, r *http.Request) {
	alerts, err := a.latestAlerts(restaurantOf(r), 60)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить уведомления")
		return
	}
	a.sendJSON(w, http.StatusOK, alerts)
}

// latestAlerts — последние уведомления ресторана (0 — ресторан не выбран, уведомлений нет).
func (a *API) latestAlerts(restaurantID int64, limit int) ([]contract.Alert, error) {
	if limit < 1 {
		limit = 5
	}
	// Ленту уведомлений спрашивает опрос каждой страницы раз в 3 секунды — она кэшируется
	// в Redis до следующего изменения данных (новое уведомление всегда меняет ревизию).
	cacheKey := "alerts:" + strconv.FormatInt(restaurantID, 10) + ":" + strconv.Itoa(limit) + ":" + strconv.FormatInt(a.currentRev(), 10)
	if cached, ok := a.KV.Get(cacheKey); ok {
		var alerts []contract.Alert
		if json.Unmarshal([]byte(cached), &alerts) == nil {
			return alerts, nil
		}
	}
	alerts, err := a.queryAlerts(restaurantID, limit)
	if err == nil {
		if encoded, encodeErr := json.Marshal(alerts); encodeErr == nil {
			a.KV.Set(cacheKey, string(encoded), 30*time.Second)
		}
	}
	return alerts, err
}

func (a *API) queryAlerts(restaurantID int64, limit int) ([]contract.Alert, error) {
	rows, err := a.DB.Query("SELECT id,at,level,text FROM alerts WHERE restaurant_id=? ORDER BY id DESC LIMIT ?", restaurantID, limit)
	if err != nil {
		return nil, err
	}
	result := make([]contract.Alert, 0)
	for rows.Next() {
		var id int64
		var at string
		var level string
		var item contract.Alert
		if err := rows.Scan(&id, &at, &level, &item.Text); err != nil {
			rows.Close()
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		item.At = parseTime(at)
		item.Level = contract.AlertLevel(level)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return result, nil
}

func (a *API) Pulse(w http.ResponseWriter, r *http.Request) {
	now, speed := a.nowAndSpeed()
	alerts, err := a.latestAlerts(restaurantOf(r), 5)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить состояние")
		return
	}
	a.sendJSON(w, http.StatusOK, contract.Pulse{
		Rev: a.currentRev(),
		Now: now,
		Speed: speed,
		Alerts: alerts,
	})
}

func (a *API) DemoState(w http.ResponseWriter, r *http.Request) {
	now, speed := a.nowAndSpeed()
	a.sendJSON(w, http.StatusOK, contract.DemoState{Now: now, Speed: speed, Shifted: a.shifted()})
}

func (a *API) DemoClock(w http.ResponseWriter, r *http.Request) {
	var req contract.ClockRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте скорость часов")
		return
	}
	if req.Speed != 1 && req.Speed != 60 && req.Speed != 600 && req.Speed != 3600 {
		a.writeError(w, http.StatusBadRequest, "Выберите скорость 1, 60, 600 или 3600")
		return
	}
	now, _ := a.nowAndSpeed()
	if err := a.setClock(now, req.Speed); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось изменить ход времени")
		return
	}
	current, speed := a.nowAndSpeed()
	a.sendJSON(w, http.StatusOK, contract.DemoState{Now: current, Speed: speed, Shifted: a.shifted()})
}

func (a *API) DemoAdvance(w http.ResponseWriter, r *http.Request) {
	var req contract.AdvanceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Укажите количество часов для перемотки")
		return
	}
	if req.Hours < 1 || req.Hours > 240 || math.IsNaN(req.Hours) || math.IsInf(req.Hours, 0) {
		a.writeError(w, http.StatusBadRequest, "Можно перемотать время от 1 до 240 часов")
		return
	}
	now, speed := a.nowAndSpeed()
	if err := a.setClock(now.Add(time.Duration(req.Hours*float64(time.Hour))), speed); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось перемотать время")
		return
	}
	current, currentSpeed := a.nowAndSpeed()
	a.sendJSON(w, http.StatusOK, contract.DemoState{Now: current, Speed: currentSpeed, Shifted: a.shifted()})
}

type seedProduct struct {
	Name string
	Unit contract.Unit
	Quantity int64
	ShelfLifeHours float64
}

type seedIngredient struct {
	Product string
	Quantity int64
}

type seedDish struct {
	Name string
	Ingredients []seedIngredient
}

func (a *API) DemoSeed(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать подготовку примера")
		return
	}
	defer tx.Rollback()
	restaurantID := restaurantOf(r)
	if err := clearOperationalData(tx, restaurantID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось очистить старые демо-данные")
		return
	}

	products := []seedProduct{
		{Name: "Куриное филе", Unit: contract.UnitGram, Quantity: 12000, ShelfLifeHours: 36},
		{Name: "Сливки", Unit: contract.UnitMilliliter, Quantity: 5000, ShelfLifeHours: 30},
		{Name: "Томаты", Unit: contract.UnitGram, Quantity: 8000, ShelfLifeHours: 20},
		{Name: "Огурцы", Unit: contract.UnitGram, Quantity: 6000, ShelfLifeHours: 14},
		{Name: "Сыр", Unit: contract.UnitGram, Quantity: 4500, ShelfLifeHours: 48},
		{Name: "Рис", Unit: contract.UnitGram, Quantity: 15000, ShelfLifeHours: 140},
		{Name: "Мука", Unit: contract.UnitGram, Quantity: 10000, ShelfLifeHours: 200},
		{Name: "Яйца", Unit: contract.UnitPiece, Quantity: 90, ShelfLifeHours: 60},
	}
	productIDs := make(map[string]int64)
	supplyNumber := "DEMO-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	result, err := tx.Exec("INSERT INTO supplies(number,created_at,restaurant_id) VALUES(?,?,?)", supplyNumber, formatTime(now), restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационную поставку")
		return
	}
	supplyID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационную поставку")
		return
	}
	for _, product := range products {
		norm := normalizeName(product.Name)
		result, err := tx.Exec("INSERT INTO products(name,norm_name,unit,restaurant_id) VALUES(?,?,?,?)", product.Name, norm, string(product.Unit), restaurantID)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационные продукты")
			return
		}
		productID, err := result.LastInsertId()
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационные продукты")
			return
		}
		productIDs[product.Name] = productID
		expires := now.Add(time.Duration(product.ShelfLifeHours * float64(time.Hour)))
		if _, err := tx.Exec(
			"INSERT INTO batches(product_id,supply_id,qty_initial,qty_remaining,written_off,received_at,expires_at,last_status) "+
				"VALUES(?,?,?,?,0,?,?,?)",
			productID, supplyID, product.Quantity, product.Quantity, formatTime(now), formatTime(expires), string(batchStatus(now, expires, now)),
		); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационные партии")
			return
		}
	}

	dishes := []seedDish{
		{Name: "Паста с курицей", Ingredients: []seedIngredient{{"Куриное филе", 150}, {"Сливки", 100}, {"Сыр", 25}}},
		{Name: "Салат с томатами", Ingredients: []seedIngredient{{"Томаты", 120}, {"Огурцы", 80}}},
		{Name: "Рис с курицей", Ingredients: []seedIngredient{{"Рис", 180}, {"Куриное филе", 120}}},
		{Name: "Сырники", Ingredients: []seedIngredient{{"Мука", 40}, {"Яйца", 1}, {"Сливки", 20}}},
		{Name: "Суп со сливками", Ingredients: []seedIngredient{{"Сливки", 150}, {"Сыр", 35}, {"Рис", 40}}},
	}
	for _, dish := range dishes {
		result, err := tx.Exec("INSERT INTO dishes(name,restaurant_id) VALUES(?,?)", dish.Name, restaurantID)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационное меню")
			return
		}
		dishID, err := result.LastInsertId()
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать демонстрационное меню")
			return
		}
		for _, ingredient := range dish.Ingredients {
			productID, exists := productIDs[ingredient.Product]
			if !exists {
				a.writeError(w, http.StatusInternalServerError, "Ошибка состава демонстрационного блюда")
				return
			}
			if _, err := tx.Exec(
				"INSERT INTO dish_ingredients(dish_id,product_id,quantity) VALUES(?,?,?)",
				dishID, productID, ingredient.Quantity,
			); err != nil {
				a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить демонстрационный рецепт")
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить демо-данные")
		return
	}
	a.bumpRev()
	_ = a.refreshStopStatuses(now)
	a.sendJSON(w, http.StatusOK, contract.DemoState{Now: now, Speed: func() float64 { _, speed := a.nowAndSpeed(); return speed }(), Shifted: a.shifted()})
}

// clearOperationalData удаляет данные одного ресторана (демо: «заполнить примером» и «сбросить»).
func clearOperationalData(tx *sql.Tx, restaurantID int64) error {
	for _, statement := range []string{
		"DELETE FROM order_writeoffs WHERE order_id IN (SELECT id FROM orders WHERE restaurant_id=?)",
		"DELETE FROM order_items WHERE order_id IN (SELECT id FROM orders WHERE restaurant_id=?)",
		"DELETE FROM orders WHERE restaurant_id=?",
		"DELETE FROM dish_ingredients WHERE dish_id IN (SELECT id FROM dishes WHERE restaurant_id=?)",
		"DELETE FROM dishes WHERE restaurant_id=?",
		"DELETE FROM batches WHERE supply_id IN (SELECT id FROM supplies WHERE restaurant_id=?)",
		"DELETE FROM supply_pending_items WHERE supply_id IN (SELECT id FROM supplies WHERE restaurant_id=?)",
		"UPDATE supply_imports SET supply_id=NULL WHERE supply_id IN (SELECT id FROM supplies WHERE restaurant_id=?)",
		"DELETE FROM supplies WHERE restaurant_id=?",
		"DELETE FROM products WHERE restaurant_id=? AND id NOT IN (SELECT product_id FROM batches)",
		"DELETE FROM alerts WHERE restaurant_id=?",
		"DELETE FROM hot_offers_manual WHERE restaurant_id=?",
		"DELETE FROM hot_offer_overrides WHERE restaurant_id=?",
	} {
		if _, err := tx.Exec(statement, restaurantID); err != nil {
			return err
		}
	}
	return nil
}

func (a *API) DemoRestock(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	restaurantID := restaurantOf(r)
	rows, err := a.DB.Query("SELECT DISTINCT p.id,p.name,p.unit FROM products p JOIN batches b ON b.product_id=p.id WHERE p.restaurant_id=?", restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось определить продукты для пополнения")
		return
	}
	type candidate struct {
		ID int64
		Name string
		Unit contract.Unit
		Available int64
	}
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.ID, &item.Name, &item.Unit); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать продукты")
			return
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать продукты")
		return
	}
	rows.Close()
	if len(candidates) == 0 {
		a.writeError(w, http.StatusBadRequest, "Сначала заполните склад примером")
		return
	}
	for i := range candidates {
		candidates[i].Available, err = a.availableProduct(candidates[i].ID, now)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать остатки")
			return
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Available == candidates[j].Available {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].Available < candidates[j].Available
	})
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}

	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать пополнение")
		return
	}
	defer tx.Rollback()
	number := "AUTO-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	result, err := tx.Exec("INSERT INTO supplies(number,created_at,restaurant_id) VALUES(?,?,?)", number, formatTime(now), restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать поставку пополнения")
		return
	}
	supplyID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать поставку пополнения")
		return
	}
	for _, item := range candidates {
		quantity := int64(5000)
		if item.Unit == contract.UnitPiece {
			quantity = 30
		}
		if _, err := tx.Exec(
			"INSERT INTO batches(product_id,supply_id,qty_initial,qty_remaining,written_off,received_at,expires_at,last_status) "+
				"VALUES(?,?,?,?,0,?,?,?)",
			item.ID, supplyID, quantity, quantity, formatTime(now), formatTime(now.Add(96*time.Hour)),
			string(contract.StatusOK),
		); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать партии пополнения")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить пополнение")
		return
	}
	a.bumpRev()
	supply, err := a.loadSupply(supplyID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Пополнение сохранено, но не удалось прочитать поставку")
		return
	}
	_ = supply // The contract returns the current demo clock state for this operation.
	current, speed := a.nowAndSpeed()
	a.sendJSON(w, http.StatusOK, contract.DemoState{Now: current, Speed: speed, Shifted: a.shifted()})
}

func (a *API) DemoReset(w http.ResponseWriter, r *http.Request) {
	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать сброс")
		return
	}
	defer tx.Rollback()
	if err := clearOperationalData(tx, restaurantOf(r)); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сбросить данные")
		return
	}
	if err := tx.Commit(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить сброс")
		return
	}
	a.bumpRev()
	if err := a.setClock(time.Now().UTC(), 1); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Данные сброшены, но не удалось вернуть часы")
		return
	}
	now, speed := a.nowAndSpeed()
	a.sendJSON(w, http.StatusOK, contract.DemoState{Now: now, Speed: speed, Shifted: false})
}

func (a *API) computeHotOffers(restaurantID int64, now time.Time) ([]contract.HotOffer, error) {
	batches, err := a.loadBatchRows("WHERE p.restaurant_id=? AND b.qty_remaining>0 ORDER BY b.expires_at ASC,b.id ASC", restaurantID)
	if err != nil {
		return nil, err
	}
	rates, _, err := a.consumptionRates(now)
	if err != nil {
		return nil, err
	}
	type candidateReason struct {
		DishID int64
		DishName string
		Reason contract.HotReason
		Score float64
		AvailablePortions int64
		Until time.Time
	}
	candidates := make([]candidateReason, 0)
	// Партии идут по возрастанию срока, поэтому здесь копится остаток более ранних партий
	// продукта: при текущем расходе они уходят первыми (FEFO).
	earlierQty := make(map[int64]float64)
	for _, batch := range batches {
		if !now.Before(batch.ExpiresAt) {
			continue
		}
		hoursLeft := batch.ExpiresAt.Sub(now).Hours()
		lifeHours := batch.ExpiresAt.Sub(batch.ReceivedAt).Hours()
		window := lifeHours * 0.35
		if window < 6 {
			window = 6
		}
		if window > 72 {
			window = 72
		}

		// Объём под риском: сколько останется к концу срока, если ранние партии расходуются первыми.
		rate := rates[batch.ProductID]
		consumable := rate*hoursLeft - earlierQty[batch.ProductID]
		if consumable < 0 {
			consumable = 0
		}
		earlierQty[batch.ProductID] += float64(batch.QtyRemaining)
		atRisk := float64(batch.QtyRemaining) - consumable
		if atRisk < 1 {
			continue
		}

		// Срочность растёт к концу срока; если партия не успевает расходоваться, она растёт и
		// по мере того, как проходит срок, — не дожидаясь последнего окна.
		urgency := 1 - hoursLeft/window
		if lifeHours > 0 {
			elapsedShare := 1 - hoursLeft/lifeHours
			if riskUrgency := elapsedShare * atRisk / float64(batch.QtyRemaining); riskUrgency > urgency {
				urgency = riskUrgency
			}
		}
		if urgency <= 0 {
			continue
		}
		rows, err := a.DB.Query(
			"SELECT d.id,d.name,di.quantity FROM dishes d JOIN dish_ingredients di ON di.dish_id=d.id "+
				"WHERE di.product_id=? ORDER BY d.name COLLATE NOCASE",
			batch.ProductID,
		)
		if err != nil {
			return nil, err
		}
		type useRow struct {
			ID int64
			Name string
			PerPortion int64
		}
		uses := make([]useRow, 0)
		for rows.Next() {
			var item useRow
			if err := rows.Scan(&item.ID, &item.Name, &item.PerPortion); err != nil {
				rows.Close()
				return nil, err
			}
			uses = append(uses, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		availableDishCount := 0
		type activeUse struct {
			useRow
			Dish contract.Dish
		}
		active := make([]activeUse, 0)
		for _, use := range uses {
			dish, err := a.buildDish(use.ID, now)
			if err != nil || dish.Stopped || dish.AvailablePortions <= 0 {
				continue
			}
			availableDishCount++
			active = append(active, activeUse{useRow: use, Dish: dish})
		}
		if availableDishCount == 0 {
			continue
		}

		for _, use := range active {
			maxPortionsToSpend := float64(use.Dish.AvailablePortions * use.PerPortion)
			coverage := math.Min(1, maxPortionsToSpend/atRisk)
			exclusivity := 1 / math.Sqrt(float64(availableDishCount))
			weight := urgency * coverage * exclusivity
			score := weight * 100
			if score < 2 {
				continue
			}
			portionCount := int64(math.Ceil(atRisk / float64(use.PerPortion)))
			if portionCount > use.Dish.AvailablePortions {
				portionCount = use.Dish.AvailablePortions
			}
			candidates = append(candidates, candidateReason{
				DishID: use.ID,
				DishName: use.Name,
				AvailablePortions: use.Dish.AvailablePortions,
				Until: batch.ExpiresAt,
				Score: score,
				Reason: contract.HotReason{
					Weight: weight,
					ProductID: strconv.FormatInt(batch.ProductID, 10),
					ProductName: batch.ProductName,
					Unit: batch.Unit,
					SupplyNumber: batch.SupplyNumber,
					ExpiresAt: batch.ExpiresAt,
					BatchQuantity: batch.QtyRemaining,
					AtRisk: int64(math.Ceil(atRisk)),
					PerPortion: use.PerPortion,
					PortionsToClear: portionCount,
					OtherDishes: availableDishCount-1,
				},
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].DishID == candidates[j].DishID {
			return candidates[i].Reason.Weight > candidates[j].Reason.Weight
		}
		return candidates[i].Score > candidates[j].Score
	})
	byDish := make(map[int64][]candidateReason)
	for _, candidate := range candidates {
		byDish[candidate.DishID] = append(byDish[candidate.DishID], candidate)
	}
	result := make([]contract.HotOffer, 0, len(byDish))
	for dishID, reasons := range byDish {
		sort.Slice(reasons, func(i, j int) bool { return reasons[i].Reason.Weight > reasons[j].Reason.Weight })
		if len(reasons) == 0 {
			continue
		}
		sum := reasons[0].Reason.Weight
		for _, reason := range reasons[1:] {
			sum += reason.Reason.Weight * 0.25
		}
		score := int(math.Round(math.Min(100, sum*100)))
		if score < 2 {
			continue
		}
		first := reasons[0]
		offer := contract.HotOffer{
			DishID: strconv.FormatInt(dishID, 10),
			Name: first.DishName,
			Score: score,
			AvailablePortions: first.AvailablePortions,
			Target: contract.HotTarget{Portions: first.Reason.PortionsToClear, Until: first.Until},
			Reasons: make([]contract.HotReason, 0, len(reasons)),
		}
		for _, reason := range reasons {
			offer.Reasons = append(offer.Reasons, reason.Reason)
		}
		result = append(result, offer)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	return result, nil
}

// HotOffers — горячие предложения ресторана: рассчитанные и добавленные вручную.
// Официант не видит скрытые; менеджер, директор и владелец видят их с пометкой hidden
// и предложения, ждущие рассмотрения.
func (a *API) HotOffers(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	offers, err := a.visibleHotOffers(restaurantOf(r), now, currentAuth(r).User)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать горячие предложения")
		return
	}
	a.sendJSON(w, http.StatusOK, offers)
}

// autoOfferID — устойчивый id рассчитанного предложения: блюдо и главная партия. Удалённое
// предложение не вернётся, пока причина та же; новая партия даст новое предложение.
func autoOfferID(offer contract.HotOffer) string {
	key := offer.DishID
	if len(offer.Reasons) > 0 {
		key += "|" + offer.Reasons[0].ProductID + "|" + offer.Reasons[0].SupplyNumber
	}
	sum := sha1.Sum([]byte(key))
	return "auto-" + hex.EncodeToString(sum[:8])
}

// visibleHotOffers собирает предложения для пользователя: сначала ручные, затем рассчитанные.
func (a *API) visibleHotOffers(restaurantID int64, now time.Time, viewer contract.User) ([]contract.HotOffer, error) {
	staff := viewer.Role != contract.RoleWaiter
	result, err := a.manualHotOffers(restaurantID, now, viewer)
	if err != nil {
		return nil, err
	}
	auto, err := a.computeHotOffers(restaurantID, now)
	if err != nil {
		return nil, err
	}
	overrides := make(map[string]string)
	rows, err := a.DB.Query("SELECT offer_id,action FROM hot_offer_overrides WHERE restaurant_id=?", restaurantID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, action string
		if err := rows.Scan(&id, &action); err != nil {
			rows.Close()
			return nil, err
		}
		overrides[id] = action
	}
	rows.Close()
	for _, offer := range auto {
		offer.ID, offer.Source, offer.Status = autoOfferID(offer), "auto", "active"
		switch overrides[offer.ID] {
		case "deleted":
			continue
		case "hidden":
			if !staff {
				continue
			}
			offer.Hidden = true
		}
		result = append(result, offer)
	}
	return result, nil
}

// activeHotOffers — то, что видно официанту как действующее: без скрытых и без ждущих решения.
// По ним меню отмечает блюда «горячими».
func (a *API) activeHotOffers(restaurantID int64, now time.Time) ([]contract.HotOffer, error) {
	offers, err := a.visibleHotOffers(restaurantID, now, contract.User{Role: contract.RoleWaiter})
	if err != nil {
		return nil, err
	}
	active := offers[:0]
	for _, offer := range offers {
		if offer.Status == "active" && !offer.Hidden {
			active = append(active, offer)
		}
	}
	return active, nil
}

// manualHotOffers — ручные предложения ресторана, которые ещё действуют. Официант видит
// действующие (кроме скрытых) и свои — на рассмотрении или отклонённые за последние сутки.
func (a *API) manualHotOffers(restaurantID int64, now time.Time, viewer contract.User) ([]contract.HotOffer, error) {
	rows, err := a.DB.Query(
		"SELECT h.id,h.dish_id,h.reason,h.target_portions,h.until,h.status,h.hidden,COALESCE(h.created_by,0),COALESCE(ac.name,''),COALESCE(h.reviewed_at,'') "+
			"FROM hot_offers_manual h LEFT JOIN accounts ac ON ac.id=h.created_by WHERE h.restaurant_id=? ORDER BY h.until,h.id",
		restaurantID,
	)
	if err != nil {
		return nil, err
	}
	type manualRow struct {
		ID, DishID, Portions, CreatedByID int64
		Reason, Until, Status, CreatedBy  string
		Reviewed                          string
		Hidden                            bool
	}
	list := make([]manualRow, 0)
	for rows.Next() {
		var row manualRow
		if err := rows.Scan(&row.ID, &row.DishID, &row.Reason, &row.Portions, &row.Until, &row.Status, &row.Hidden, &row.CreatedByID, &row.CreatedBy, &row.Reviewed); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, row)
	}
	rows.Close()

	staff := viewer.Role != contract.RoleWaiter
	own := func(row manualRow) bool { return strconv.FormatInt(row.CreatedByID, 10) == viewer.ID }
	result := make([]contract.HotOffer, 0)
	for _, row := range list {
		until := parseTime(row.Until)
		if !now.Before(until) {
			continue
		}
		switch row.Status {
		case "active":
			if row.Hidden && !staff {
				continue
			}
		case "pending":
			if !staff && !own(row) {
				continue
			}
		case "rejected":
			if !own(row) || now.Sub(parseTime(row.Reviewed)) > 24*time.Hour {
				continue
			}
		default:
			continue
		}
		dish, err := a.buildDish(row.DishID, now)
		if err != nil {
			continue
		}
		result = append(result, contract.HotOffer{
			ID: "manual-" + strconv.FormatInt(row.ID, 10), Source: "manual", Status: row.Status, Hidden: row.Hidden,
			Note: row.Reason, CreatedBy: row.CreatedBy,
			DishID: dish.DishID, Name: dish.Name, Score: 100, AvailablePortions: dish.AvailablePortions,
			Target:  contract.HotTarget{Portions: row.Portions, Until: until},
			Reasons: []contract.HotReason{},
		})
	}
	return result, nil
}

// CreateHotOffer — ручное горячее предложение: от официанта — на рассмотрение менеджеру,
// от менеджера, директора и владельца — сразу действующее.
func (a *API) CreateHotOffer(w http.ResponseWriter, r *http.Request) {
	auth := currentAuth(r)
	restaurantID := restaurantOf(r)
	var req contract.CreateHotOfferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте блюдо, причину, цель и срок")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len([]rune(req.Reason)) > 300 {
		a.writeError(w, http.StatusBadRequest, "Напишите причину — до 300 символов")
		return
	}
	if req.Portions < 1 || req.Portions > 10000 {
		a.writeError(w, http.StatusBadRequest, "Цель — от 1 до 10 000 порций")
		return
	}
	now, _ := a.nowAndSpeed()
	if !req.Until.After(now) || req.Until.After(now.Add(7*24*time.Hour)) {
		a.writeError(w, http.StatusBadRequest, "Срок — в будущем и не дальше 7 суток")
		return
	}
	dishID, err := parseID(req.DishID)
	var dishName string
	if err != nil || a.DB.QueryRow("SELECT name FROM dishes WHERE id=? AND restaurant_id=?", dishID, restaurantID).Scan(&dishName) != nil {
		a.writeError(w, http.StatusBadRequest, "Выберите блюдо из меню ресторана")
		return
	}
	status := "active"
	if auth.User.Role == contract.RoleWaiter {
		status = "pending"
	}
	if _, err := a.DB.Exec(
		"INSERT INTO hot_offers_manual(restaurant_id,dish_id,reason,target_portions,until,status,created_by,created_at) VALUES(?,?,?,?,?,?,?,?)",
		restaurantID, dishID, req.Reason, req.Portions, formatTime(req.Until.UTC()), status, auth.User.ID, formatTime(now),
	); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить горячее предложение")
		return
	}
	if status == "pending" {
		a.addAlert(restaurantID, contract.AlertLevel("warning"), fmt.Sprintf("Официант %s предложил горячее предложение «%s» — рассмотрите его.", auth.User.Name, dishName), now)
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusCreated, map[string]string{"status": status})
}

// manualOfferID разбирает «manual-N».
func manualOfferID(id string) (int64, bool) {
	if !strings.HasPrefix(id, "manual-") {
		return 0, false
	}
	value, err := parseID(strings.TrimPrefix(id, "manual-"))
	return value, err == nil
}

// ReviewHotOffer — менеджер, директор или владелец одобряет или отклоняет предложение официанта.
func (a *API) ReviewHotOffer(w http.ResponseWriter, r *http.Request) {
	id, ok := manualOfferID(r.PathValue("id"))
	var req struct {
		Action string `json:"action"`
	}
	if !ok || decodeJSON(w, r, &req) != nil || (req.Action != "approve" && req.Action != "reject") {
		a.writeError(w, http.StatusBadRequest, "Укажите решение: approve или reject")
		return
	}
	status := map[string]string{"approve": "active", "reject": "rejected"}[req.Action]
	now, _ := a.nowAndSpeed()
	result, err := a.DB.Exec("UPDATE hot_offers_manual SET status=?,reviewed_at=? WHERE id=? AND restaurant_id=? AND status='pending'",
		status, formatTime(now), id, restaurantOf(r))
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить решение")
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		a.writeError(w, http.StatusNotFound, "Предложение не найдено или уже рассмотрено")
		return
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusOK, map[string]string{"status": status})
}

// HideHotOffer скрывает предложение от официантов (hidden=true) или показывает снова.
func (a *API) HideHotOffer(w http.ResponseWriter, r *http.Request) {
	offerID := r.PathValue("id")
	restaurantID := restaurantOf(r)
	var req struct {
		Hidden bool `json:"hidden"`
	}
	if decodeJSON(w, r, &req) != nil {
		a.writeError(w, http.StatusBadRequest, "Укажите hidden: true или false")
		return
	}
	now, _ := a.nowAndSpeed()
	var err error
	if id, ok := manualOfferID(offerID); ok {
		var result sql.Result
		result, err = a.DB.Exec("UPDATE hot_offers_manual SET hidden=? WHERE id=? AND restaurant_id=?", req.Hidden, id, restaurantID)
		if err == nil {
			if count, _ := result.RowsAffected(); count == 0 {
				a.writeError(w, http.StatusNotFound, "Предложение не найдено")
				return
			}
		}
	} else if strings.HasPrefix(offerID, "auto-") {
		if req.Hidden {
			_, err = a.DB.Exec("INSERT INTO hot_offer_overrides(restaurant_id,offer_id,action,created_at) VALUES(?,?,'hidden',?) "+
				"ON CONFLICT(restaurant_id,offer_id) DO UPDATE SET action='hidden'", restaurantID, offerID, formatTime(now))
		} else {
			_, err = a.DB.Exec("DELETE FROM hot_offer_overrides WHERE restaurant_id=? AND offer_id=? AND action='hidden'", restaurantID, offerID)
		}
	} else {
		a.writeError(w, http.StatusNotFound, "Предложение не найдено")
		return
	}
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить изменение")
		return
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusOK, map[string]bool{"hidden": req.Hidden})
}

// DeleteHotOffer удаляет ручное предложение или убирает рассчитанное (пока не изменится причина).
func (a *API) DeleteHotOffer(w http.ResponseWriter, r *http.Request) {
	offerID := r.PathValue("id")
	restaurantID := restaurantOf(r)
	now, _ := a.nowAndSpeed()
	var err error
	if id, ok := manualOfferID(offerID); ok {
		var result sql.Result
		result, err = a.DB.Exec("DELETE FROM hot_offers_manual WHERE id=? AND restaurant_id=?", id, restaurantID)
		if err == nil {
			if count, _ := result.RowsAffected(); count == 0 {
				a.writeError(w, http.StatusNotFound, "Предложение не найдено")
				return
			}
		}
	} else if strings.HasPrefix(offerID, "auto-") {
		_, err = a.DB.Exec("INSERT INTO hot_offer_overrides(restaurant_id,offer_id,action,created_at) VALUES(?,?,'deleted',?) "+
			"ON CONFLICT(restaurant_id,offer_id) DO UPDATE SET action='deleted'", restaurantID, offerID, formatTime(now))
	} else {
		a.writeError(w, http.StatusNotFound, "Предложение не найдено")
		return
	}
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить предложение")
		return
	}
	a.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) consumptionRates(now time.Time) (map[int64]float64, map[int64]time.Time, error) {
	rows, err := a.DB.Query(
		"SELECT ow.product_id,ow.quantity,o.created_at FROM order_writeoffs ow "+
			"JOIN orders o ON o.id=ow.order_id WHERE o.status='accepted'",
	)
	if err != nil {
		return nil, nil, err
	}
	totals := make(map[int64]float64)
	first := make(map[int64]time.Time)
	for rows.Next() {
		var productID string
		var quantity int64
		var created string
		if err := rows.Scan(&productID, &quantity, &created); err != nil {
			rows.Close()
			return nil, nil, err
		}
		id, err := parseID(productID)
		if err != nil {
			continue
		}
		at := parseTime(created)
		if at.After(now) || now.Sub(at) > 24*time.Hour {
			continue
		}
		totals[id] += float64(quantity)
		if old, ok := first[id]; !ok || at.Before(old) {
			first[id] = at
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	rates := make(map[int64]float64)
	for id, total := range totals {
		window := now.Sub(first[id]).Hours()
		if window < 1 {
			window = 1
		}
		if window > 24 {
			window = 24
		}
		rates[id] = total / window
	}
	return rates, first, nil
}

func (a *API) Analytics(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	result := contract.Analytics{
		Writeoffs: make([]contract.Writeoffs, 0),
		Deficits: make([]contract.Deficit, 0),
		Forecast: make([]contract.Forecast, 0),
		Purchases: make([]contract.Purchase, 0),
	}
	restaurantID := restaurantOf(r)
	if err := a.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status='accepted' AND restaurant_id=?", restaurantID).Scan(&result.Orders.Accepted); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать аналитику")
		return
	}
	if err := a.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status='rejected' AND restaurant_id=?", restaurantID).Scan(&result.Orders.Rejected); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать аналитику")
		return
	}
	if err := a.DB.QueryRow("SELECT COUNT(*) FROM batches b JOIN products p ON p.id=b.product_id WHERE b.written_off>0 AND p.restaurant_id=?", restaurantID).Scan(&result.WriteoffEvents); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать списания")
		return
	}

	rows, err := a.DB.Query(
		"SELECT p.id,p.name,p.unit,COALESCE(SUM(b.written_off),0),COALESCE(SUM(b.qty_initial),0) "+
			"FROM products p JOIN batches b ON b.product_id=p.id WHERE p.restaurant_id=? GROUP BY p.id,p.name,p.unit",
		restaurantID,
	)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать списания")
		return
	}
	for rows.Next() {
		var id int64
		var item contract.Writeoffs
		if err := rows.Scan(&id, &item.Name, &item.Unit, &item.Quantity, &item.Received); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать списания")
			return
		}
		item.ProductID = strconv.FormatInt(id, 10)
		if item.Received > 0 {
			item.Share = float64(item.Quantity) / float64(item.Received)
		}
		if item.Quantity > 0 {
			result.Writeoffs = append(result.Writeoffs, item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать списания")
		return
	}
	rows.Close()
	sort.Slice(result.Writeoffs, func(i, j int) bool { return result.Writeoffs[i].Share > result.Writeoffs[j].Share })

	rows, err = a.DB.Query("SELECT shortages_json FROM orders WHERE status='rejected' AND restaurant_id=?", restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать дефициты")
		return
	}
	deficitMap := make(map[string]*contract.Deficit)
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать дефициты")
			return
		}
		var shortages []contract.Shortage
		if json.Unmarshal([]byte(encoded), &shortages) != nil {
			continue
		}
		seen := make(map[string]bool)
		for _, shortage := range shortages {
			item := deficitMap[shortage.ProductID]
			if item == nil {
				item = &contract.Deficit{
					ProductID: shortage.ProductID,
					Name: shortage.ProductName,
					Unit: shortage.Unit,
				}
				deficitMap[shortage.ProductID] = item
			}
			item.Missing += shortage.Missing
			if !seen[shortage.ProductID] {
				item.Rejections++
				seen[shortage.ProductID] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать дефициты")
		return
	}
	rows.Close()
	for _, item := range deficitMap {
		result.Deficits = append(result.Deficits, *item)
	}
	sort.Slice(result.Deficits, func(i, j int) bool { return result.Deficits[i].Rejections > result.Deficits[j].Rejections })

	rates, _, err := a.consumptionRates(now)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать расход продуктов")
		return
	}
	productRows, err := a.DB.Query("SELECT id,name,unit FROM products WHERE restaurant_id=? ORDER BY name COLLATE NOCASE", restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать закупки")
		return
	}
	type productData struct {
		ID int64
		Name string
		Unit contract.Unit
	}
	products := make([]productData, 0)
	for productRows.Next() {
		var p productData
		if err := productRows.Scan(&p.ID, &p.Name, &p.Unit); err != nil {
			productRows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать продукты")
			return
		}
		products = append(products, p)
	}
	if err := productRows.Err(); err != nil {
		productRows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать продукты")
		return
	}
	productRows.Close()

	maxTenPortions := make(map[int64]int64)
	minFivePortions := make(map[int64]int64)
	dishRows, err := a.DB.Query("SELECT di.dish_id,di.product_id,di.quantity FROM dish_ingredients di JOIN dishes d ON d.id=di.dish_id WHERE d.restaurant_id=?", restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать рецепты")
		return
	}
	for dishRows.Next() {
		var dishID, productID, quantity int64
		if err := dishRows.Scan(&dishID, &productID, &quantity); err != nil {
			dishRows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать рецепты")
			return
		}
		if quantity*10 > maxTenPortions[productID] {
			maxTenPortions[productID] = quantity * 10
		}
		if quantity*5 > minFivePortions[productID] {
			minFivePortions[productID] = quantity * 5
		}
	}
	if err := dishRows.Err(); err != nil {
		dishRows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать рецепты")
		return
	}
	dishRows.Close()

	for _, product := range products {
		available, err := a.availableProduct(product.ID, now)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать остатки")
			return
		}
		rate := rates[product.ID]
		if rate > 0 {
			hours := float64(available) / rate
			if hours < 48 {
				quantity := int64(math.Ceil(rate*72 - float64(available)))
				if quantity < 0 {
					quantity = 0
				}
				left := hours
				result.Purchases = append(result.Purchases, contract.Purchase{
					ProductID: strconv.FormatInt(product.ID, 10),
					Name: product.Name,
					Unit: product.Unit,
					Usable: available,
					HoursLeft: &left,
					PerDay: rate*24,
					Quantity: quantity,
					Basis: contract.PurchaseBasis("rate"),
				})
			}
		} else if maxTenPortions[product.ID] > 0 && available < minFivePortions[product.ID] {
			quantity := maxTenPortions[product.ID] - available
			if quantity < 0 {
				quantity = 0
			}
			result.Purchases = append(result.Purchases, contract.Purchase{
				ProductID: strconv.FormatInt(product.ID, 10),
				Name: product.Name,
				Unit: product.Unit,
				Usable: available,
				HoursLeft: nil,
				PerDay: 0,
				Quantity: quantity,
				Basis: contract.PurchaseBasis("recipes"),
			})
		}
	}

	// Build a practical forecast for recipes with either sales history or a batch that
	// is due to expire before the next portion can be prepared.
	menuRows, err := a.DB.Query("SELECT id FROM dishes WHERE restaurant_id=? ORDER BY name COLLATE NOCASE", restaurantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать прогноз стоп-листа")
		return
	}
	dishIDs := make([]int64, 0)
	for menuRows.Next() {
		var id int64
		if err := menuRows.Scan(&id); err != nil {
			menuRows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать меню")
			return
		}
		dishIDs = append(dishIDs, id)
	}
	if err := menuRows.Err(); err != nil {
		menuRows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать меню")
		return
	}
	menuRows.Close()
	for _, dishID := range dishIDs {
		dish, err := a.buildDish(dishID, now)
		if err != nil {
			continue
		}
		forecast := contract.Forecast{DishID: dish.DishID, Name: dish.Name}
		if dish.Stopped {
			forecast.Stopped = true
			forecast.Hours = 0
			forecast.Cause = contract.ForecastCause("now")
			if dish.StopReason != nil {
				forecast.ProductName = dish.StopReason.ProductName
			}
			forecast.At = now
			result.Forecast = append(result.Forecast, forecast)
			continue
		}
		bestHours := math.Inf(1)
		for _, ingredient := range dish.Ingredients {
			productID, parseErr := parseID(ingredient.ProductID)
			if parseErr != nil {
				continue
			}
			available, availErr := a.availableProduct(productID, now)
			if availErr != nil {
				continue
			}
			if rate := rates[productID]; rate > 0 {
				hours := float64(available-ingredient.Quantity) / rate
				if hours < 0 {
					hours = 0
				}
				if hours < bestHours {
					bestHours = hours
					forecast.Cause = contract.ForecastCause("consumption")
					forecast.ProductName = ingredient.Name
				}
			} else {
				batches, batchErr := a.loadBatchRows("WHERE b.product_id=? AND b.qty_remaining>0", productID)
				if batchErr != nil {
					continue
				}
				for _, batch := range batches {
					if !now.Before(batch.ExpiresAt) {
						continue
					}
					if available-batch.QtyRemaining < ingredient.Quantity {
						hours := batch.ExpiresAt.Sub(now).Hours()
						if hours < bestHours {
							bestHours = hours
							forecast.Cause = contract.ForecastCause("expiry")
							forecast.ProductName = ingredient.Name
						}
					}
				}
			}
		}
		if !math.IsInf(bestHours, 1) {
			forecast.Stopped = false
			forecast.Hours = bestHours
			forecast.At = now.Add(time.Duration(bestHours * float64(time.Hour)))
			result.Forecast = append(result.Forecast, forecast)
		}
	}
	sort.Slice(result.Forecast, func(i, j int) bool { return result.Forecast[i].Hours < result.Forecast[j].Hours })
	sort.Slice(result.Purchases, func(i, j int) bool {
		if result.Purchases[i].HoursLeft == nil {
			return result.Purchases[j].HoursLeft != nil
		}
		if result.Purchases[j].HoursLeft == nil {
			return true
		}
		return *result.Purchases[i].HoursLeft < *result.Purchases[j].HoursLeft
	})

	a.sendJSON(w, http.StatusOK, result)
}

// Ensure the SQL package remains used by error comparisons in this file when some
// analytics sections are trimmed in future revisions.
func _unusedAnalyticsImports() {
	_ = sql.ErrNoRows
	_ = strings.TrimSpace
}
