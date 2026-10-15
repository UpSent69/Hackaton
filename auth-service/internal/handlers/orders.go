package handlers

import (
	"auth-service/internal/contract"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *API) buildDish(id int64, now time.Time) (contract.Dish, error) {
	var dish contract.Dish
	var stoppedSince sql.NullString
	var price sql.NullInt64
	if err := a.DB.QueryRow("SELECT id,name,stopped_since,price_kopecks FROM dishes WHERE id=?", id).
		Scan(&id, &dish.Name, &stoppedSince, &price); err != nil {
		return dish, err
	}
	dish.PriceConfigured = price.Valid
	dish.PriceKopecks = price.Int64
	dish.DishID = strconv.FormatInt(id, 10)
	dish.Ingredients = make([]contract.Ingredient, 0)
	rows, err := a.DB.Query(
		"SELECT p.id,p.name,p.unit,di.quantity FROM dish_ingredients di "+
			"JOIN products p ON p.id=di.product_id WHERE di.dish_id=? ORDER BY p.name COLLATE NOCASE",
		id,
	)
	if err != nil {
		return dish, err
	}
	type ingRow struct {
		ID int64
		Name string
		Unit contract.Unit
		Quantity int64
	}
	ingredients := make([]ingRow, 0)
	for rows.Next() {
		var item ingRow
		if err := rows.Scan(&item.ID, &item.Name, &item.Unit, &item.Quantity); err != nil {
			rows.Close()
			return dish, err
		}
		ingredients = append(ingredients, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return dish, err
	}
	rows.Close()

	lowest := int64(0)
	var limiter *contract.Limit
	for index, raw := range ingredients {
		available, err := a.availableProduct(raw.ID, now)
		if err != nil {
			return dish, err
		}
		dish.Ingredients = append(dish.Ingredients, contract.Ingredient{
			ProductID: strconv.FormatInt(raw.ID, 10),
			Name: raw.Name,
			Unit: raw.Unit,
			Quantity: raw.Quantity,
		})
		portions := available / raw.Quantity
		if index == 0 || portions < lowest {
			lowest = portions
			limiter = &contract.Limit{
				ProductID: strconv.FormatInt(raw.ID, 10),
				ProductName: raw.Name,
				Unit: raw.Unit,
				Required: raw.Quantity,
				Available: available,
			}
		}
	}
	if len(ingredients) == 0 {
		dish.AvailablePortions = 0
		dish.Stopped = true
	} else {
		dish.AvailablePortions = lowest
		dish.Stopped = lowest == 0
	}
	dish.LimitedBy = limiter
	if dish.Stopped {
		dish.StopReason = limiter
	}
	if stoppedSince.Valid && stoppedSince.String != "" {
		value := parseTime(stoppedSince.String)
		dish.StoppedSince = &value
	}
	dish.Hot = false
	dish.HotScore = 0
	return dish, nil
}

func (a *API) Menu(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	rows, err := a.DB.Query("SELECT id FROM dishes ORDER BY name COLLATE NOCASE,id")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить меню")
		return
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать меню")
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать меню")
		return
	}
	rows.Close()

	menu := make([]contract.Dish, 0, len(ids))
	for _, id := range ids {
		dish, err := a.buildDish(id, now)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать доступность блюда")
			return
		}
		menu = append(menu, dish)
	}
	offers, err := a.computeHotOffers(now)
	if err == nil {
		scores := make(map[string]int, len(offers))
		for _, offer := range offers {
			scores[offer.DishID] = offer.Score
		}
		for i := range menu {
			if score, ok := scores[menu[i].DishID]; ok {
				menu[i].Hot = true
				menu[i].HotScore = score
			}
		}
	}
	a.sendJSON(w, http.StatusOK, menu)
}

func (a *API) CreateDish(w http.ResponseWriter, r *http.Request) {
	var req contract.CreateDishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте формат блюда")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 160 {
		a.writeError(w, http.StatusBadRequest, "Укажите название блюда длиной до 160 символов")
		return
	}
	if len(req.Ingredients) == 0 {
		a.writeError(w, http.StatusBadRequest, "Добавьте ингредиенты блюда")
		return
	}
	seen := make(map[int64]bool)
	ingredientIDs := make([]int64, 0, len(req.Ingredients))
	for _, ingredient := range req.Ingredients {
		id, err := parseID(ingredient.ProductID)
		if err != nil || ingredient.Quantity <= 0 || seen[id] {
			a.writeError(w, http.StatusBadRequest, "Укажите уникальные продукты и положительное количество на порцию")
			return
		}
		var exists int
		if err := a.DB.QueryRow("SELECT COUNT(*) FROM products WHERE id=?", id).Scan(&exists); err != nil || exists == 0 {
			a.writeError(w, http.StatusBadRequest, "Один из ингредиентов не найден на складе")
			return
		}
		seen[id] = true
		ingredientIDs = append(ingredientIDs, id)
	}
	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать создание блюда")
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec("INSERT INTO dishes(name) VALUES(?)", req.Name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			a.writeError(w, http.StatusConflict, "Блюдо с таким названием уже существует")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать блюдо")
		return
	}
	dishID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать блюдо")
		return
	}
	for index, id := range ingredientIDs {
		if _, err := tx.Exec(
			"INSERT INTO dish_ingredients(dish_id,product_id,quantity) VALUES(?,?,?)",
			dishID, id, req.Ingredients[index].Quantity,
		); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить ингредиенты блюда")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить блюдо")
		return
	}
	a.bumpRev()
	now, _ := a.nowAndSpeed()
	_ = a.refreshStopStatuses(now)
	dish, err := a.buildDish(dishID, now)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Блюдо создано, но не удалось прочитать его")
		return
	}
	a.sendJSON(w, http.StatusOK, dish)
}

func (a *API) DeleteDish(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор блюда")
		return
	}
	result, err := a.DB.Exec("DELETE FROM dishes WHERE id=?", id)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить блюдо")
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		a.writeError(w, http.StatusNotFound, "Блюдо не найдено")
		return
	}
	a.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}

type productRequirement struct {
	ID int64
	Name string
	Unit contract.Unit
	Required int64
	Dishes map[string]bool
}

type orderLine struct {
	DishID int64
	Name string
	Portions int64
}

func (a *API) orderRequirements(req contract.OrderRequest) ([]orderLine, map[int64]*productRequirement, error) {
	if len(req.Items) == 0 || len(req.Items) > 100 {
		return nil, nil, fmt.Errorf("Добавьте от 1 до 100 позиций в заказ")
	}
	lines := make([]orderLine, 0, len(req.Items))
	required := make(map[int64]*productRequirement)
	for _, input := range req.Items {
		dishID, err := parseID(input.DishID)
		if err != nil || input.Portions <= 0 || input.Portions > 100000 {
			return nil, nil, fmt.Errorf("Укажите корректное блюдо и положительное количество порций")
		}
		var name string
		if err := a.DB.QueryRow("SELECT name FROM dishes WHERE id=?", dishID).Scan(&name); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil, fmt.Errorf("Одно из блюд больше не существует")
			}
			return nil, nil, fmt.Errorf("Не удалось прочитать состав заказа")
		}
		lines = append(lines, orderLine{DishID: dishID, Name: name, Portions: input.Portions})
		rows, err := a.DB.Query(
			"SELECT p.id,p.name,p.unit,di.quantity FROM dish_ingredients di "+
				"JOIN products p ON p.id=di.product_id WHERE di.dish_id=?",
			dishID,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("Не удалось прочитать состав блюда")
		}
		type ingredientRow struct {
			ID int64
			Name string
			Unit contract.Unit
			Quantity int64
		}
		ingredients := make([]ingredientRow, 0)
		for rows.Next() {
			var ingredient ingredientRow
			if err := rows.Scan(&ingredient.ID, &ingredient.Name, &ingredient.Unit, &ingredient.Quantity); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("Не удалось прочитать состав блюда")
			}
			ingredients = append(ingredients, ingredient)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("Не удалось прочитать состав блюда")
		}
		rows.Close()
		if len(ingredients) == 0 {
			return nil, nil, fmt.Errorf("У блюда «%s» не настроены ингредиенты", name)
		}
		for _, ingredient := range ingredients {
			item := required[ingredient.ID]
			if item == nil {
				item = &productRequirement{
					ID: ingredient.ID, Name: ingredient.Name, Unit: ingredient.Unit,
					Dishes: make(map[string]bool),
				}
				required[ingredient.ID] = item
			}
			item.Required += ingredient.Quantity * input.Portions
			item.Dishes[name] = true
		}
	}
	return lines, required, nil
}

func (a *API) makeShortages(required map[int64]*productRequirement, now time.Time) ([]contract.Shortage, error) {
	ids := make([]int64, 0, len(required))
	for id := range required {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return strings.ToLower(required[ids[i]].Name) < strings.ToLower(required[ids[j]].Name)
	})
	result := make([]contract.Shortage, 0)
	for _, id := range ids {
		item := required[id]
		available, err := a.availableProduct(id, now)
		if err != nil {
			return nil, err
		}
		if available >= item.Required {
			continue
		}
		dishes := make([]string, 0, len(item.Dishes))
		for name := range item.Dishes {
			dishes = append(dishes, name)
		}
		sort.Strings(dishes)
		result = append(result, contract.Shortage{
			ProductID: strconv.FormatInt(item.ID, 10),
			ProductName: item.Name,
			Unit: item.Unit,
			Required: item.Required,
			Available: available,
			Missing: item.Required - available,
			Dishes: dishes,
		})
	}
	return result, nil
}

func (a *API) ValidateOrder(w http.ResponseWriter, r *http.Request) {
	var req contract.OrderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте формат заказа")
		return
	}
	_, required, err := a.orderRequirements(req)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now, _ := a.nowAndSpeed()
	shortages, err := a.makeShortages(required, now)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить остатки")
		return
	}
	a.sendJSON(w, http.StatusOK, contract.ValidateResult{OK: len(shortages) == 0, Shortages: shortages})
}

func txAvailable(tx *sql.Tx, productID int64, now time.Time) (int64, error) {
	rows, err := tx.Query("SELECT qty_remaining,expires_at FROM batches WHERE product_id=? AND qty_remaining>0", productID)
	if err != nil {
		return 0, err
	}
	var total int64
	for rows.Next() {
		var quantity int64
		var expires string
		if err := rows.Scan(&quantity, &expires); err != nil {
			rows.Close()
			return 0, err
		}
		if now.Before(parseTime(expires)) {
			total += quantity
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	return total, nil
}

func shortagesForTx(tx *sql.Tx, required map[int64]*productRequirement, now time.Time) ([]contract.Shortage, error) {
	ids := make([]int64, 0, len(required))
	for id := range required {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return strings.ToLower(required[ids[i]].Name) < strings.ToLower(required[ids[j]].Name)
	})
	result := make([]contract.Shortage, 0)
	for _, id := range ids {
		item := required[id]
		available, err := txAvailable(tx, id, now)
		if err != nil {
			return nil, err
		}
		if available >= item.Required {
			continue
		}
		dishes := make([]string, 0, len(item.Dishes))
		for name := range item.Dishes {
			dishes = append(dishes, name)
		}
		sort.Strings(dishes)
		result = append(result, contract.Shortage{
			ProductID: strconv.FormatInt(item.ID, 10),
			ProductName: item.Name,
			Unit: item.Unit,
			Required: item.Required,
			Available: available,
			Missing: item.Required - available,
			Dishes: dishes,
		})
	}
	return result, nil
}

func (a *API) CreateOrder(w http.ResponseWriter, r *http.Request) {
	var req contract.OrderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте формат заказа")
		return
	}
	lines, required, err := a.orderRequirements(req)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now, _ := a.nowAndSpeed()
	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать оформление заказа")
		return
	}
	defer tx.Rollback()

	shortages, err := shortagesForTx(tx, required, now)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить остатки")
		return
	}
	var nextNumber int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(order_number),0)+1 FROM orders").Scan(&nextNumber); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось назначить номер заказа")
		return
	}
	status := contract.OrderAccepted
	if len(shortages) > 0 {
		status = contract.OrderRejected
	}
	result, err := tx.Exec(
		"INSERT INTO orders(order_number,created_at,status,shortages_json) VALUES(?,?,?,?)",
		nextNumber, formatTime(now), string(status), jsonString(shortages),
	)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить заказ")
		return
	}
	orderID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить заказ")
		return
	}
	for _, line := range lines {
		if _, err := tx.Exec(
			"INSERT INTO order_items(order_id,dish_id,name,portions) VALUES(?,?,?,?)",
			orderID, strconv.FormatInt(line.DishID, 10), line.Name, line.Portions,
		); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить позиции заказа")
			return
		}
	}

	if len(shortages) == 0 {
		productIDs := make([]int64, 0, len(required))
		for id := range required {
			productIDs = append(productIDs, id)
		}
		sort.Slice(productIDs, func(i, j int) bool { return productIDs[i] < productIDs[j] })
		for _, productID := range productIDs {
			requirement := required[productID]
			remaining := requirement.Required
			rows, err := tx.Query(
				"SELECT b.id,b.qty_remaining,b.expires_at,s.number FROM batches b "+
					"JOIN supplies s ON s.id=b.supply_id WHERE b.product_id=? AND b.qty_remaining>0 "+
					"ORDER BY b.expires_at ASC,b.id ASC",
				productID,
			)
			if err != nil {
				a.writeError(w, http.StatusInternalServerError, "Не удалось выбрать партии для списания")
				return
			}
			type allocationBatch struct {
				ID int64
				Remaining int64
				Expires time.Time
				SupplyNumber string
			}
			availableBatches := make([]allocationBatch, 0)
			for rows.Next() {
				var b allocationBatch
				var expires string
				if err := rows.Scan(&b.ID, &b.Remaining, &expires, &b.SupplyNumber); err != nil {
					rows.Close()
					a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать партии")
					return
				}
				b.Expires = parseTime(expires)
				if now.Before(b.Expires) {
					availableBatches = append(availableBatches, b)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать партии")
				return
			}
			rows.Close()

			for _, batch := range availableBatches {
				if remaining <= 0 {
					break
				}
				quantity := batch.Remaining
				if quantity > remaining {
					quantity = remaining
				}
				if _, err := tx.Exec("UPDATE batches SET qty_remaining=qty_remaining-? WHERE id=?", quantity, batch.ID); err != nil {
					a.writeError(w, http.StatusInternalServerError, "Не удалось списать продукт")
					return
				}
				if _, err := tx.Exec(
					"INSERT INTO order_writeoffs(order_id,product_id,product_name,unit,batch_id,supply_number,expires_at,quantity) "+
						"VALUES(?,?,?,?,?,?,?,?)",
					orderID, strconv.FormatInt(productID, 10), requirement.Name, string(requirement.Unit),
					strconv.FormatInt(batch.ID, 10), batch.SupplyNumber, formatTime(batch.Expires), quantity,
				); err != nil {
					a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить списание")
					return
				}
				remaining -= quantity
			}
			if remaining > 0 {
				a.writeError(w, http.StatusConflict, "Остаток изменился во время оформления. Повторите заказ")
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось завершить заказ")
		return
	}
	a.bumpRev()
	if len(shortages) > 0 {
		a.writeErrorWithShortages(w, http.StatusConflict, "Недостаточно продуктов для оформления заказа", shortages)
		return
	}
	order, err := a.loadOrder(orderID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Заказ оформлен, но не удалось прочитать его")
		return
	}
	a.sendJSON(w, http.StatusOK, order)
}

func (a *API) loadOrder(id int64) (contract.Order, error) {
	var order contract.Order
	var created, status, shortagesJSON string
	if err := a.DB.QueryRow(
		"SELECT id,order_number,created_at,status,shortages_json FROM orders WHERE id=?", id,
	).Scan(&id, &order.Number, &created, &status, &shortagesJSON); err != nil {
		return order, err
	}
	order.ID = strconv.FormatInt(id, 10)
	order.CreatedAt = parseTime(created)
	order.Status = contract.OrderStatus(status)
	order.Items = make([]contract.OrderLine, 0)
	order.Shortages = make([]contract.Shortage, 0)
	order.Writeoffs = make([]contract.Writeoff, 0)
	if err := json.Unmarshal([]byte(shortagesJSON), &order.Shortages); err != nil || order.Shortages == nil {
		order.Shortages = make([]contract.Shortage, 0)
	}
	rows, err := a.DB.Query("SELECT dish_id,name,portions FROM order_items WHERE order_id=? ORDER BY id", id)
	if err != nil {
		return order, err
	}
	for rows.Next() {
		var item contract.OrderLine
		if err := rows.Scan(&item.DishID, &item.Name, &item.Portions); err != nil {
			rows.Close()
			return order, err
		}
		order.Items = append(order.Items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return order, err
	}
	rows.Close()

	rows, err = a.DB.Query(
		"SELECT product_id,product_name,unit,batch_id,supply_number,expires_at,quantity "+
			"FROM order_writeoffs WHERE order_id=? ORDER BY id",
		id,
	)
	if err != nil {
		return order, err
	}
	for rows.Next() {
		var item contract.Writeoff
		var expires string
		if err := rows.Scan(
			&item.ProductID, &item.ProductName, &item.Unit, &item.BatchID,
			&item.SupplyNumber, &expires, &item.Quantity,
		); err != nil {
			rows.Close()
			return order, err
		}
		item.ExpiresAt = parseTime(expires)
		order.Writeoffs = append(order.Writeoffs, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return order, err
	}
	rows.Close()
	return order, nil
}

func (a *API) Orders(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query("SELECT id FROM orders ORDER BY order_number DESC,id DESC")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить историю заказов")
		return
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать историю заказов")
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать историю заказов")
		return
	}
	rows.Close()

	result := make([]contract.Order, 0, len(ids))
	for _, id := range ids {
		order, err := a.loadOrder(id)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать заказ")
			return
		}
		result = append(result, order)
	}
	a.sendJSON(w, http.StatusOK, result)
}
