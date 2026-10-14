package handlers

import (
	"auth-service/internal/contract"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type batchRow struct {
	ID           int64
	ProductID    int64
	SupplyID     int64
	SupplyNumber string
	ProductName  string
	Unit         contract.Unit
	QtyInitial   int64
	QtyRemaining int64
	WrittenOff   int64
	ReceivedAt   time.Time
	ExpiresAt    time.Time
	LastStatus   string
}

func batchStatus(receivedAt, expiresAt, now time.Time) contract.ExpiryStatus {
	if expiresAt.IsZero() || !now.Before(expiresAt) {
		return contract.StatusExpired
	}
	life := expiresAt.Sub(receivedAt)
	remaining := expiresAt.Sub(now)
	if life <= 0 {
		return contract.StatusExpired
	}
	share := float64(remaining) / float64(life)
	if share < 0.10 || remaining < 2*time.Hour {
		return contract.StatusCritical
	}
	if share < 0.30 || remaining < 6*time.Hour {
		return contract.StatusWarning
	}
	return contract.StatusOK
}

func statusRank(status contract.ExpiryStatus) int {
	switch status {
	case contract.StatusExpired:
		return 4
	case contract.StatusCritical:
		return 3
	case contract.StatusWarning:
		return 2
	default:
		return 1
	}
}

func (a *API) loadBatchRows(where string, args ...interface{}) ([]batchRow, error) {
	query := "SELECT b.id,b.product_id,b.supply_id,s.number,p.name,p.unit," +
		"b.qty_initial,b.qty_remaining,b.written_off,b.received_at,b.expires_at,b.last_status " +
		"FROM batches b JOIN products p ON p.id=b.product_id JOIN supplies s ON s.id=b.supply_id " + where
	rows, err := a.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	result := make([]batchRow, 0)
	for rows.Next() {
		var item batchRow
		var received, expires string
		if err := rows.Scan(
			&item.ID, &item.ProductID, &item.SupplyID, &item.SupplyNumber, &item.ProductName, &item.Unit,
			&item.QtyInitial, &item.QtyRemaining, &item.WrittenOff, &received, &expires, &item.LastStatus,
		); err != nil {
			rows.Close()
			return nil, err
		}
		item.ReceivedAt = parseTime(received)
		item.ExpiresAt = parseTime(expires)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return result, nil
}

func (a *API) addAlert(level contract.AlertLevel, text string, at time.Time) {
	_, _ = a.DB.Exec("INSERT INTO alerts(at,level,text) VALUES(?,?,?)", formatTime(at), string(level), text)
}

func (a *API) maintain() error {
	now, _ := a.nowAndSpeed()
	batches, err := a.loadBatchRows("")
	if err != nil {
		return err
	}
	changed := false
	for _, batch := range batches {
		status := batchStatus(batch.ReceivedAt, batch.ExpiresAt, now)
		if !now.Before(batch.ExpiresAt) && batch.QtyRemaining > 0 {
			_, err := a.DB.Exec(
				"UPDATE batches SET qty_remaining=0,written_off=written_off+?,last_status=? WHERE id=?",
				batch.QtyRemaining, string(contract.StatusExpired), batch.ID,
			)
			if err != nil {
				return err
			}
			a.addAlert(contract.AlertLevel("expired"), fmt.Sprintf("Срок продукта «%s» из поставки %s истёк: остаток списан.", batch.ProductName, batch.SupplyNumber), now)
			changed = true
			continue
		}
		if batch.LastStatus != string(status) {
			_, err := a.DB.Exec("UPDATE batches SET last_status=? WHERE id=?", string(status), batch.ID)
			if err != nil {
				return err
			}
			if batch.LastStatus != "" {
				switch status {
				case contract.StatusWarning:
					a.addAlert(contract.AlertLevel("warning"), fmt.Sprintf("У продукта «%s» из поставки %s скоро истекает срок.", batch.ProductName, batch.SupplyNumber), now)
				case contract.StatusCritical:
					a.addAlert(contract.AlertLevel("critical"), fmt.Sprintf("У продукта «%s» из поставки %s критически близкий срок годности.", batch.ProductName, batch.SupplyNumber), now)
				}
			}
			changed = true
		}
	}
	if changed {
		a.bumpRev()
	}
	return a.refreshStopStatuses(now)
}

func (a *API) refreshStopStatuses(now time.Time) error {
	rows, err := a.DB.Query("SELECT id,name,stopped_since FROM dishes ORDER BY id")
	if err != nil {
		return err
	}
	type dishState struct {
		ID int64
		Name string
		StoppedSince sql.NullString
	}
	dishes := make([]dishState, 0)
	for rows.Next() {
		var d dishState
		if err := rows.Scan(&d.ID, &d.Name, &d.StoppedSince); err != nil {
			rows.Close()
			return err
		}
		dishes = append(dishes, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	changed := false
	for _, old := range dishes {
		dish, err := a.buildDish(old.ID, now)
		if err != nil {
			continue
		}
		wasStopped := old.StoppedSince.Valid && old.StoppedSince.String != ""
		if dish.Stopped && !wasStopped {
			_, err := a.DB.Exec("UPDATE dishes SET stopped_since=? WHERE id=?", formatTime(now), old.ID)
			if err != nil {
				return err
			}
			a.addAlert(contract.AlertLevel("stop"), fmt.Sprintf("Блюдо «%s» попало в стоп-лист: не хватает ингредиента.", old.Name), now)
			changed = true
		} else if !dish.Stopped && wasStopped {
			_, err := a.DB.Exec("UPDATE dishes SET stopped_since=NULL WHERE id=?", old.ID)
			if err != nil {
				return err
			}
			a.addAlert(contract.AlertLevel("ok"), fmt.Sprintf("Блюдо «%s» снова доступно.", old.Name), now)
			changed = true
		}
	}
	if changed {
		a.bumpRev()
	}
	return nil
}

func (a *API) availableProduct(productID int64, now time.Time) (int64, error) {
	batches, err := a.loadBatchRows("WHERE b.product_id=?", productID)
	if err != nil {
		return 0, err
	}
	var available int64
	for _, batch := range batches {
		if now.Before(batch.ExpiresAt) && batch.QtyRemaining > 0 {
			available += batch.QtyRemaining
		}
	}
	return available, nil
}

func (a *API) Products(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query("SELECT id,name,unit,base_price_kopecks FROM products ORDER BY name COLLATE NOCASE")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить продукты")
		return
	}
	defer rows.Close()
	products := make([]contract.Product, 0)
	for rows.Next() {
		var id int64
		var price sql.NullInt64
		var product contract.Product
		if err := rows.Scan(&id, &product.Name, &product.Unit, &price); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать продукты")
			return
		}
		product.ID = strconv.FormatInt(id, 10)
		product.PriceConfigured = price.Valid
		if price.Valid {
			product.BasePriceKopecks = price.Int64
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать продукты")
		return
	}
	if err := rows.Close(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось закрыть каталог продуктов")
		return
	}
	now, _ := a.nowAndSpeed()
	availableByProduct, err := a.availableStockByProduct(now)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать остатки склада")
		return
	}
	for i := range products {
		productID, parseErr := strconv.ParseInt(products[i].ID, 10, 64)
		if parseErr == nil {
			products[i].Available = availableByProduct[productID]
		}
	}
	a.sendJSON(w, http.StatusOK, products)
}

func (a *API) Stock(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	rows, err := a.DB.Query(
		"SELECT DISTINCT p.id,p.name,p.unit FROM products p JOIN batches b ON b.product_id=p.id ORDER BY p.name COLLATE NOCASE",
	)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить склад")
		return
	}
	type productRow struct {
		ID int64
		Name string
		Unit contract.Unit
	}
	products := make([]productRow, 0)
	for rows.Next() {
		var product productRow
		if err := rows.Scan(&product.ID, &product.Name, &product.Unit); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать склад")
			return
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать склад")
		return
	}
	rows.Close()

	result := make([]contract.StockItem, 0, len(products))
	for _, product := range products {
		batches, err := a.loadBatchRows("WHERE b.product_id=?", product.ID)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось рассчитать остатки")
			return
		}
		item := contract.StockItem{
			ProductID: strconv.FormatInt(product.ID, 10),
			Name: product.Name,
			Unit: product.Unit,
			Status: contract.StatusOK,
		}
		for _, batch := range batches {
			item.BatchCount++
			status := batchStatus(batch.ReceivedAt, batch.ExpiresAt, now)
			if statusRank(status) > statusRank(item.Status) {
				item.Status = status
			}
			if now.Before(batch.ExpiresAt) && batch.QtyRemaining > 0 {
				item.Available += batch.QtyRemaining
				if item.NearestExpiresAt == nil || batch.ExpiresAt.Before(*item.NearestExpiresAt) {
					expiry := batch.ExpiresAt
					item.NearestExpiresAt = &expiry
				}
			} else if batch.QtyRemaining > 0 {
				item.Expired += batch.QtyRemaining
			}
		}
		result = append(result, item)
	}
	a.sendJSON(w, http.StatusOK, result)
}

func (a *API) Batches(w http.ResponseWriter, r *http.Request) {
	productID, err := parseID(r.PathValue("productId"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор продукта")
		return
	}
	var exists int
	if err := a.DB.QueryRow("SELECT COUNT(*) FROM products WHERE id=?", productID).Scan(&exists); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось найти продукт")
		return
	}
	if exists == 0 {
		a.writeError(w, http.StatusNotFound, "Продукт не найден")
		return
	}
	rows, err := a.loadBatchRows("WHERE b.product_id=? ORDER BY b.expires_at ASC,b.id ASC", productID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить партии")
		return
	}
	result := make([]contract.Batch, 0, len(rows))
	now, _ := a.nowAndSpeed()
	for _, row := range rows {
		result = append(result, contract.Batch{
			ID: strconv.FormatInt(row.ID, 10),
			ProductID: strconv.FormatInt(row.ProductID, 10),
			SupplyID: strconv.FormatInt(row.SupplyID, 10),
			SupplyNumber: row.SupplyNumber,
			QtyInitial: row.QtyInitial,
			QtyRemaining: row.QtyRemaining,
			ReceivedAt: row.ReceivedAt,
			ExpiresAt: row.ExpiresAt,
			Status: batchStatus(row.ReceivedAt, row.ExpiresAt, now),
		})
	}
	a.sendJSON(w, http.StatusOK, result)
}

func (a *API) Expiry(w http.ResponseWriter, r *http.Request) {
	now, _ := a.nowAndSpeed()
	batches, err := a.loadBatchRows("WHERE b.qty_remaining>0 OR b.written_off>0 ORDER BY b.expires_at ASC,b.id ASC")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить сроки годности")
		return
	}
	result := make([]contract.ExpiryBatch, 0, len(batches))
	for _, row := range batches {
		result = append(result, contract.ExpiryBatch{
			Batch: contract.Batch{
				ID: strconv.FormatInt(row.ID, 10),
				ProductID: strconv.FormatInt(row.ProductID, 10),
				SupplyID: strconv.FormatInt(row.SupplyID, 10),
				SupplyNumber: row.SupplyNumber,
				QtyInitial: row.QtyInitial,
				QtyRemaining: row.QtyRemaining,
				ReceivedAt: row.ReceivedAt,
				ExpiresAt: row.ExpiresAt,
				Status: batchStatus(row.ReceivedAt, row.ExpiresAt, now),
			},
			Name: row.ProductName,
			Unit: row.Unit,
			WrittenOff: row.WrittenOff,
		})
	}
	a.sendJSON(w, http.StatusOK, result)
}

func (a *API) Supplies(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query("SELECT id,number,created_at FROM supplies ORDER BY created_at DESC,id DESC")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить поставки")
		return
	}
	type supplyHead struct {
		ID int64
		Number string
		Created time.Time
	}
	heads := make([]supplyHead, 0)
	for rows.Next() {
		var h supplyHead
		var created string
		if err := rows.Scan(&h.ID, &h.Number, &created); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать поставки")
			return
		}
		h.Created = parseTime(created)
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать поставки")
		return
	}
	rows.Close()

	result := make([]contract.Supply, 0, len(heads))
	for _, h := range heads {
		supply, err := a.loadSupply(h.ID)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить состав поставки")
			return
		}
		result = append(result, supply)
	}
	a.sendJSON(w, http.StatusOK, result)
}

func (a *API) loadSupply(id int64) (contract.Supply, error) {
	var supply contract.Supply
	var created string
	if err := a.DB.QueryRow("SELECT id,number,created_at FROM supplies WHERE id=?", id).
		Scan(&id, &supply.Number, &created); err != nil {
		return supply, err
	}
	supply.ID = strconv.FormatInt(id, 10)
	supply.CreatedAt = parseTime(created)
	rows, err := a.DB.Query(
		"SELECT p.id,p.name,p.unit,b.qty_initial,b.expires_at,b.lot_number FROM batches b "+
			"JOIN products p ON p.id=b.product_id WHERE b.supply_id=? ORDER BY p.name COLLATE NOCASE,b.id",
		id,
	)
	if err != nil {
		return supply, err
	}
	supply.Items = make([]contract.SupplyItem, 0)
	for rows.Next() {
		var productID int64
		var item contract.SupplyItem
		var expires string
		if err := rows.Scan(&productID, &item.Name, &item.Unit, &item.Quantity, &expires, &item.LotNumber); err != nil {
			rows.Close()
			return supply, err
		}
		item.ProductID = strconv.FormatInt(productID, 10)
		item.ExpiresAt = parseTime(expires)
		supply.Items = append(supply.Items, item)
	}
	err = rows.Err()
	rows.Close()
	return supply, err
}

func (a *API) CreateSupply(w http.ResponseWriter, r *http.Request) {
	var req contract.CreateSupplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверьте формат поставки")
		return
	}
	req.Number = strings.TrimSpace(req.Number)
	if req.Number == "" || len(req.Number) > 80 {
		a.writeError(w, http.StatusBadRequest, "Укажите номер поставки длиной до 80 символов")
		return
	}
	if len(req.Items) == 0 {
		a.writeError(w, http.StatusBadRequest, "Добавьте хотя бы один продукт в поставку")
		return
	}
	seen := make(map[string]bool)
	for _, item := range req.Items {
		key := normalizeName(item.Name)
		if key == "" || len(item.Name) > 160 || !validUnit(item.Unit) || item.Quantity <= 0 ||
			item.ShelfLifeHours <= 0 || math.IsNaN(item.ShelfLifeHours) || math.IsInf(item.ShelfLifeHours, 0) ||
			item.ShelfLifeHours > 87600 || len(strings.TrimSpace(item.LotNumber)) > 40 {
			a.writeError(w, http.StatusBadRequest, "Проверьте названия, единицы, количества и сроки хранения продуктов")
			return
		}
		if seen[key] {
			a.writeError(w, http.StatusBadRequest, "Один продукт нельзя дважды добавить в одну поставку")
			return
		}
		seen[key] = true
	}

	var importID int64
	if strings.TrimSpace(req.ImportID) != "" {
		var parseErr error
		if importID, parseErr = parseID(req.ImportID); parseErr != nil {
			a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор импорта поставки")
			return
		}
	}

	now, _ := a.nowAndSpeed()
	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать запись поставки")
		return
	}
	defer tx.Rollback()

	result, err := tx.Exec("INSERT INTO supplies(number,created_at) VALUES(?,?)", req.Number, formatTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			a.writeError(w, http.StatusConflict, "Поставка с таким номером уже существует")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать поставку")
		return
	}
	supplyID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать поставку")
		return
	}

	for _, item := range req.Items {
		norm := normalizeName(item.Name)
		var productID int64
		var productName string
		var productUnit contract.Unit
		err := tx.QueryRow("SELECT id,name,unit FROM products WHERE norm_name=?", norm).Scan(&productID, &productName, &productUnit)
		if err == sql.ErrNoRows {
			productName = strings.TrimSpace(item.Name)
			productUnit = item.Unit
			res, insertErr := tx.Exec("INSERT INTO products(name,norm_name,unit) VALUES(?,?,?)", productName, norm, string(productUnit))
			if insertErr != nil {
				a.writeError(w, http.StatusInternalServerError, "Не удалось создать продукт")
				return
			}
			productID, err = res.LastInsertId()
			if err != nil {
				a.writeError(w, http.StatusInternalServerError, "Не удалось создать продукт")
				return
			}
		} else if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось найти продукт")
			return
		} else if productUnit != item.Unit {
			a.writeError(w, http.StatusBadRequest, fmt.Sprintf("Для продукта «%s» уже задана другая единица измерения", productName))
			return
		}

		expires := now.Add(time.Duration(item.ShelfLifeHours * float64(time.Hour)))
		status := batchStatus(now, expires, now)
		lot := strings.TrimSpace(item.LotNumber)
		if lot == "" {
			lot = randomLotNumber()
		}
		if _, err := tx.Exec(
			"INSERT INTO batches(product_id,supply_id,qty_initial,qty_remaining,written_off,received_at,expires_at,last_status,lot_number) "+
				"VALUES(?,?,?,?,0,?,?,?,?)",
			productID, supplyID, item.Quantity, item.Quantity, formatTime(now), formatTime(expires), string(status), lot,
		); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось создать партию продукта")
			return
		}
	}
	// The import journal row is linked in the same transaction, so a saved supply always
	// points back to the file it was recognised from.
	if importID > 0 {
		if _, err := tx.Exec(
			"UPDATE supply_imports SET supply_id=?,saved_at=? WHERE id=? AND supply_id IS NULL",
			supplyID, formatTime(now), importID,
		); err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось связать поставку с импортом")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить поставку")
		return
	}
	a.bumpRev()
	supply, err := a.loadSupply(supplyID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Поставка сохранена, но её не удалось прочитать")
		return
	}
	a.sendJSON(w, http.StatusCreated, supply)
}

func (a *API) DeleteSupply(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор поставки")
		return
	}
	result, err := a.DB.Exec("DELETE FROM supplies WHERE id=?", id)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить поставку")
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		a.writeError(w, http.StatusNotFound, "Поставка не найдена")
		return
	}
	a.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) DeleteSupplyItem(w http.ResponseWriter, r *http.Request) {
	supplyID, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор поставки")
		return
	}
	productID, err := parseID(r.PathValue("productId"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор продукта")
		return
	}
	result, err := a.DB.Exec("DELETE FROM batches WHERE supply_id=? AND product_id=?", supplyID, productID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось удалить продукт из поставки")
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		a.writeError(w, http.StatusNotFound, "Позиция поставки не найдена")
		return
	}
	var left int
	if err := a.DB.QueryRow("SELECT COUNT(*) FROM batches WHERE supply_id=?", supplyID).Scan(&left); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить поставку")
		return
	}
	if left == 0 {
		_, _ = a.DB.Exec("DELETE FROM supplies WHERE id=?", supplyID)
	}
	a.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}

// Keep sorting deterministic if multiple batches have identical expiry timestamps.
func sortBatchRows(rows []batchRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ExpiresAt.Equal(rows[j].ExpiresAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].ExpiresAt.Before(rows[j].ExpiresAt)
	})
}

func _unusedWarehouseImports() {
	_ = sql.ErrNoRows
	_ = sortBatchRows
	_ = strconv.IntSize
	_ = strings.TrimSpace
}
