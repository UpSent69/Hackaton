package handlers

import (
	"auth-service/internal/contract"
	"auth-service/internal/requestimport"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxRequestUpload = 12 << 20

type requestCatalogProduct struct {
	ID               int64
	Name             string
	Unit             string
	BasePriceKopecks int64
	PriceConfigured  bool
	Available        int64
}

// requestPreviewItem — строка черновика заявки: блюдо меню и число порций.
type requestPreviewItem struct {
	OriginalName        string  `json:"originalName"`
	DishName            string  `json:"dishName"`
	DishID              string  `json:"dishId"`
	CatalogMatch        bool    `json:"catalogMatch"` // блюдо найдено в меню
	Portions            int64   `json:"portions"`
	AvailablePortions   int64   `json:"availablePortions"` // сколько можно приготовить из остатков сейчас
	UnitPriceKopecks    int64   `json:"unitPriceKopecks"`
	PriceConfigured     bool    `json:"priceConfigured"`
	ItemDiscountPercent int     `json:"itemDiscountPercent"`
	Confidence          float64 `json:"confidence"`
}

type parsedRequestResponse struct {
	CustomerName   string               `json:"customerName"`
	SourceFilename string               `json:"sourceFilename"`
	SourceHash     string               `json:"sourceHash"`
	AIUsed         bool                 `json:"aiUsed"`
	ParserMode     string               `json:"parserMode"`
	DuplicateSaved bool                 `json:"duplicateSaved"`
	Items          []requestPreviewItem `json:"items"`
	Warnings       []string             `json:"warnings"`
	Catalog        []contract.Dish      `json:"catalog"` // меню с ценами порций
}

type requestCatalogDTO struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Unit             string `json:"unit"`
	BasePriceKopecks int64  `json:"basePriceKopecks"`
	PriceConfigured  bool   `json:"priceConfigured"`
	Available        int64  `json:"available"`
}

type createRequestInput struct {
	CustomerName          string              `json:"customerName"`
	SourceFilename        string              `json:"sourceFilename"`
	SourceHash            string              `json:"sourceHash"`
	GlobalDiscountPercent int                 `json:"globalDiscountPercent"`
	Items                 []createRequestItem `json:"items"`
}

type createRequestItem struct {
	DishID              string `json:"dishId"`
	Portions            int64  `json:"portions"`
	ItemDiscountPercent int    `json:"itemDiscountPercent"`
	UnitPriceKopecks    *int64 `json:"unitPriceKopecks"` // только для блюда без цены порции
}

type savedRequestItem struct {
	DishID              string `json:"dishId"`
	DishName            string `json:"dishName"`
	Portions            int64  `json:"portions"`
	UnitPriceKopecks    int64  `json:"unitPriceKopecks"`
	ItemDiscountPercent int    `json:"itemDiscountPercent"`
	SubtotalKopecks     int64  `json:"subtotalKopecks"`
	TotalKopecks        int64  `json:"totalKopecks"`
}

type savedRequest struct {
	ID                    string             `json:"id"`
	Number                string             `json:"number"`
	CustomerName          string             `json:"customerName"`
	SourceFilename        string             `json:"sourceFilename"`
	CreatedAt             time.Time          `json:"createdAt"`
	GlobalDiscountPercent int                `json:"globalDiscountPercent"`
	SubtotalKopecks       int64              `json:"subtotalKopecks"`
	TotalKopecks          int64              `json:"totalKopecks"`
	DiscountKopecks       int64              `json:"discountKopecks"`
	Status                string             `json:"status"`
	Items                 []savedRequestItem `json:"items"`
}

func (a *API) requestCatalog() ([]requestCatalogProduct, error) {
	rows, err := a.DB.Query("SELECT id,name,unit,base_price_kopecks FROM products ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}

	products := make([]requestCatalogProduct, 0)
	for rows.Next() {
		var product requestCatalogProduct
		var price sql.NullInt64
		if err := rows.Scan(&product.ID, &product.Name, &product.Unit, &price); err != nil {
			rows.Close()
			return nil, err
		}
		product.PriceConfigured = price.Valid
		if price.Valid {
			product.BasePriceKopecks = price.Int64
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	now, _ := a.nowAndSpeed()
	availableByProduct, err := a.availableStockByProduct(now)
	if err != nil {
		return nil, err
	}
	for i := range products {
		products[i].Available = availableByProduct[products[i].ID]
	}
	return products, nil
}

// availableStockByProduct counts only non-expired remaining stock in the app's current clock.
func (a *API) availableStockByProduct(now time.Time) (map[int64]int64, error) {
	batches, err := a.loadBatchRows("")
	if err != nil {
		return nil, err
	}
	available := make(map[int64]int64)
	for _, batch := range batches {
		if batch.QtyRemaining > 0 && now.Before(batch.ExpiresAt) {
			available[batch.ProductID] += batch.QtyRemaining
		}
	}
	return available, nil
}

func catalogDTO(products []requestCatalogProduct) []requestCatalogDTO {
	result := make([]requestCatalogDTO, 0, len(products))
	for _, product := range products {
		result = append(result, requestCatalogDTO{
			ID: strconv.FormatInt(product.ID, 10), Name: product.Name, Unit: product.Unit,
			BasePriceKopecks: product.BasePriceKopecks, PriceConfigured: product.PriceConfigured, Available: product.Available,
		})
	}
	return result
}

// ParseRequestFile reads the upload in memory, asks the local model to extract a draft,
// and enriches each row from the catalogue. No request is written to the database here.
func (a *API) ParseRequestFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestUpload+(1<<20))
	if err := r.ParseMultipartForm(maxRequestUpload); err != nil {
		a.writeError(w, http.StatusBadRequest, "Не удалось прочитать файл. Максимальный размер — 12 МБ.")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Выбери файл заявки")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxRequestUpload+1))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Не удалось прочитать загруженный файл")
		return
	}
	if len(data) > maxRequestUpload {
		a.writeError(w, http.StatusRequestEntityTooLarge, "Размер файла превышает 12 МБ")
		return
	}

	filename := safeRequestFilename(header.Filename)
	source, err := requestimport.Extract(filename, data)
	if err != nil {
		a.writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])
	log.Printf(
		"Импорт заявки: файл=%s, размер=%d байт, текст=%d символов, изображение=%d байт",
		filename, len(data), len([]rune(source.Text)), len(source.Image),
	)

	// Простые списки разбираются сразу, всё остальное — через очередь Qwen.
	if isPlainTable(filename, source) {
		if document := requestimport.Fallback(source.Text); len(document.Items) > 0 {
			names := make([]string, 0, len(document.Items))
			for _, item := range document.Items {
				names = append(names, item.ProductName)
			}
			if !needsModelMatch(names, a.dishNames()) {
				a.finishRequestParse(r.Context(), w, filename, hashHex, source)
				return
			}
		}
	}
	a.enqueueDocument(w, "request", filename, hashHex, source)
}

// finishRequestParse распознаёт заявку и сопоставляет позиции с каталогом. Заявка здесь
// не сохраняется. Вызывается из запроса (простые списки) или из очереди документов.
func (a *API) finishRequestParse(ctx context.Context, w http.ResponseWriter, filename, hashHex string, source requestimport.Source) {
	baseURL, model := requestimport.OllamaConfig()
	warnings := make([]string, 0)
	var document requestimport.Document
	var aiErr error
	aiUsed := false
	parserMode := "ai"

	// Small text lists are faster and more reliable with a deterministic parser;
	// don't wait for a local LLM to interpret a simple "name quantity unit" row.
	if isPlainTable(filename, source) {
		candidate := requestimport.Fallback(source.Text)
		if len(candidate.Items) > 0 {
			document = candidate
			parserMode = "structured"
		}
	}

	if parserMode != "structured" {
		aiCtx, cancel := context.WithTimeout(ctx, jobTimeout)
		started := time.Now()
		document, aiErr = requestimport.Analyze(aiCtx, source, baseURL, model)
		elapsed := time.Since(started)
		cancel()
		log.Printf("Анализ Ollama завершён: время=%s, ошибка=%v", elapsed, aiErr)

		if aiErr != nil {
			log.Printf("Ошибка анализа через Ollama: %v", aiErr)
			document = requestimport.Fallback(source.Text)
			parserMode = "fallback"
			warnings = append(warnings, "Нейросеть не ответила — включён упрощённый разбор. Проверь каждую строку вручную.")
		} else if len(document.Items) == 0 && source.Text != "" {
			// A local model may return syntactically valid JSON but no positions.
			// In that case, don't silently show an empty draft if rows are readable.
			candidate := requestimport.Fallback(source.Text)
			parserMode = "fallback"
			if len(candidate.Items) > 0 {
				document = candidate
				warnings = append(warnings, "Нейросеть не выделила позиции; строки восстановлены упрощённым разбором. Проверь распознавание.")
			}
		}
		if parserMode == "ai" && aiErr == nil && len(document.Items) > 0 {
			aiUsed = true
		}
	}

	log.Printf("Импорт заявки завершён: режим=%s, позиций=%d", parserMode, len(document.Items))
	if len(document.Items) == 0 {
		warnings = append(warnings, "Не удалось уверенно выделить позиции. Их можно добавить вручную.")
	}
	if source.ImageMIME == "" && strings.EqualFold(filepath.Ext(filename), ".pdf") {
		// A text-based PDF was extracted. Scanned pages are rejected by Extract with a clear message.
	}

	dishes, err := a.menuDishes()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить меню")
		return
	}
	menu := make([]string, len(dishes))
	for i, dish := range dishes {
		menu[i] = dish.Name
	}
	names := make([]string, 0, len(document.Items))
	for _, item := range document.Items {
		names = append(names, item.ProductName)
	}
	// Поиск по смыслу: «Салат из помидоров» → «Салат с томатами»; чего нет в меню — помечается.
	matches := a.matchNames(ctx, names, menu, "dish")
	preview := make([]requestPreviewItem, 0, len(document.Items))
	notInMenu := make(map[string]bool)
	for _, item := range document.Items {
		portions := int64(math.Ceil(item.Quantity))
		if portions < 1 {
			portions = 1
		}
		row := requestPreviewItem{
			OriginalName: strings.TrimSpace(item.ProductName),
			DishName:     strings.TrimSpace(item.ProductName),
			Portions:     portions,
			Confidence:   item.Confidence,
		}
		if index, ok := matches[item.ProductName]; ok {
			dish := dishes[index]
			row.CatalogMatch = true
			row.DishID = dish.DishID
			row.DishName = dish.Name
			row.AvailablePortions = dish.AvailablePortions
			row.UnitPriceKopecks = dish.PriceKopecks
			row.PriceConfigured = dish.PriceConfigured
		} else {
			notInMenu[row.OriginalName] = true
		}
		preview = append(preview, row)
	}
	if len(notInMenu) > 0 {
		warnings = append(warnings, "Этих блюд нет в меню: "+strings.Join(requestimport.SortedKeys(notInMenu), ", ")+". Выберите замену из меню или удалите строку.")
	}
	duplicate := false
	if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM customer_requests WHERE source_sha256=?)", hashHex).Scan(&duplicate); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить повторную загрузку")
		return
	}
	a.sendJSON(w, http.StatusOK, parsedRequestResponse{
		CustomerName: strings.TrimSpace(document.CustomerName), SourceFilename: filename, SourceHash: hashHex,
		AIUsed: aiUsed, DuplicateSaved: duplicate, Items: preview, Warnings: uniqueStrings(warnings),
		Catalog: dishes, ParserMode: parserMode,
	})
}

func matchRequestProduct(name string, products []requestCatalogProduct) (requestCatalogProduct, bool) {
	normalized := normalizeRequestName(name)
	if normalized == "" {
		return requestCatalogProduct{}, false
	}
	for _, product := range products {
		if normalized == normalizeRequestName(product.Name) {
			return product, true
		}
	}
	var candidate requestCatalogProduct
	found := 0
	for _, product := range products {
		catalogName := normalizeRequestName(product.Name)
		if len(catalogName) >= 4 && len(normalized) >= 4 &&
			(strings.Contains(normalized, catalogName) || strings.Contains(catalogName, normalized)) {
			candidate = product
			found++
		}
	}
	return candidate, found == 1
}

func normalizeRequestName(value string) string {
	value = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(value, "ё", "е"), "×", "x"))
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func safeRequestFilename(filename string) string {
	filename = strings.ReplaceAll(filename, string([]byte{92}), "/")
	filename = filepath.Base(filename)
	filename = strings.TrimSpace(filename)
	if filename == "" || filename == "." {
		return "заявка"
	}
	if len(filename) > 240 {
		filename = filename[len(filename)-240:]
	}
	return filename
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

// UpdateProductPrice edits only the catalogue price. A quote never trusts prices submitted by the browser.
func (a *API) UpdateProductPrice(w http.ResponseWriter, r *http.Request) {
	productID, err := parseID(r.PathValue("productId"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор товара")
		return
	}
	var input struct {
		PriceKopecks int64 `json:"priceKopecks"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		a.writeError(w, http.StatusBadRequest, "Передай цену в копейках")
		return
	}
	if input.PriceKopecks < 0 || input.PriceKopecks > 1000000000000 {
		a.writeError(w, http.StatusBadRequest, "Цена должна быть от 0 до 10 000 000 000 ₽")
		return
	}
	result, err := a.DB.Exec("UPDATE products SET base_price_kopecks=? WHERE id=?", input.PriceKopecks, productID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось обновить цену")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		a.writeError(w, http.StatusNotFound, "Товар не найден")
		return
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusOK, map[string]any{
		"id": strconv.FormatInt(productID, 10), "priceKopecks": input.PriceKopecks, "priceConfigured": true,
	})
}

// UpdateDishPrice задаёт цену порции блюда — по ней считаются заявки на банкеты.
func (a *API) UpdateDishPrice(w http.ResponseWriter, r *http.Request) {
	dishID, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный идентификатор блюда")
		return
	}
	var input struct {
		PriceKopecks int64 `json:"priceKopecks"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		a.writeError(w, http.StatusBadRequest, "Передай цену в копейках")
		return
	}
	if input.PriceKopecks < 0 || input.PriceKopecks > 1000000000000 {
		a.writeError(w, http.StatusBadRequest, "Цена должна быть от 0 до 10 000 000 000 ₽")
		return
	}
	result, err := a.DB.Exec("UPDATE dishes SET price_kopecks=? WHERE id=?", input.PriceKopecks, dishID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось обновить цену")
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		a.writeError(w, http.StatusNotFound, "Блюдо не найдено")
		return
	}
	a.bumpRev()
	a.sendJSON(w, http.StatusOK, map[string]any{
		"id": strconv.FormatInt(dishID, 10), "priceKopecks": input.PriceKopecks, "priceConfigured": true,
	})
}

// dishNames — названия блюд меню.
func (a *API) dishNames() []string {
	requestMu.Lock()
	defer requestMu.Unlock()
	rows, err := a.DB.Query("SELECT name FROM dishes ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			names = append(names, name)
		}
	}
	return names
}

func (a *API) CreateRequest(w http.ResponseWriter, r *http.Request) {
	var input createRequestInput
	if err := decodeJSON(w, r, &input); err != nil {
		a.writeError(w, http.StatusBadRequest, "Проверь формат заявки")
		return
	}
	input.CustomerName = strings.TrimSpace(input.CustomerName)
	if len(input.CustomerName) > 200 {
		a.writeError(w, http.StatusBadRequest, "Имя заказчика не должно превышать 200 символов")
		return
	}
	if input.CustomerName == "" {
		input.CustomerName = "Не указан"
	}
	input.SourceFilename = safeRequestFilename(input.SourceFilename)
	if input.SourceFilename == "заявка" {
		input.SourceFilename = ""
	}
	input.SourceHash = strings.TrimSpace(strings.ToLower(input.SourceHash))
	if input.SourceHash != "" && (len(input.SourceHash) != 64 || strings.Trim(input.SourceHash, "0123456789abcdef") != "") {
		a.writeError(w, http.StatusBadRequest, "Некорректный хеш исходного файла")
		return
	}
	if input.GlobalDiscountPercent < 0 || input.GlobalDiscountPercent > 100 {
		a.writeError(w, http.StatusBadRequest, "Общая скидка должна быть от 0 до 100%")
		return
	}
	if len(input.Items) == 0 || len(input.Items) > 500 {
		a.writeError(w, http.StatusBadRequest, "Добавь от 1 до 500 блюд")
		return
	}
	if input.SourceHash != "" {
		var duplicate int
		if err := a.DB.QueryRow("SELECT id FROM customer_requests WHERE source_sha256=?", input.SourceHash).Scan(&duplicate); err == nil {
			a.writeError(w, http.StatusConflict, "Заявка из этого файла уже сохранена")
			return
		} else if !errors.Is(err, sql.ErrNoRows) {
			a.writeError(w, http.StatusInternalServerError, "Не удалось проверить дубликаты заявок")
			return
		}
	}

	tx, err := a.DB.Begin()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось начать сохранение заявки")
		return
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	tempNumber := fmt.Sprintf("TMP-%d", now.UnixNano())
	result, err := tx.Exec(
		"INSERT INTO customer_requests(request_number,customer_name,source_filename,source_sha256,global_discount_percent,subtotal_kopecks,total_kopecks,created_at,status) VALUES(?,?,?,?,?,0,0,?,'saved')",
		tempNumber, input.CustomerName, input.SourceFilename, input.SourceHash, input.GlobalDiscountPercent, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			a.writeError(w, http.StatusConflict, "Заявка из этого файла уже сохранена")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось создать заявку")
		return
	}
	requestID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось получить номер заявки")
		return
	}
	requestNumber := fmt.Sprintf("REQ-%s-%06d", now.Format("20060102"), requestID)
	if _, err := tx.Exec("UPDATE customer_requests SET request_number=? WHERE id=?", requestNumber, requestID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось назначить номер заявки")
		return
	}

	var subtotalTotal int64
	var grandTotal int64
	for _, item := range input.Items {
		dishID, err := parseID(item.DishID)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Для каждой строки выбери блюдо из меню")
			return
		}
		if item.Portions <= 0 || item.Portions > 100000 {
			a.writeError(w, http.StatusBadRequest, "Число порций должно быть от 1 до 100 000")
			return
		}
		if item.ItemDiscountPercent < 0 || item.ItemDiscountPercent > 100 {
			a.writeError(w, http.StatusBadRequest, "Скидка на строку должна быть от 0 до 100%")
			return
		}

		var dishName string
		var price sql.NullInt64
		if err := tx.QueryRow("SELECT name,price_kopecks FROM dishes WHERE id=?", dishID).Scan(&dishName, &price); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				a.writeError(w, http.StatusBadRequest, "Одно из выбранных блюд больше нет в меню")
			} else {
				a.writeError(w, http.StatusInternalServerError, "Не удалось проверить меню")
			}
			return
		}
		var unitPriceKopecks int64
		if price.Valid {
			// Цена порции из меню всегда важнее цены, присланной браузером.
			unitPriceKopecks = price.Int64
		} else {
			if item.UnitPriceKopecks == nil {
				a.writeError(w, http.StatusBadRequest, "Для блюда «"+dishName+"» не задана цена порции. Введи цену вручную в строке заявки.")
				return
			}
			if *item.UnitPriceKopecks < 0 || *item.UnitPriceKopecks > 1000000000000 {
				a.writeError(w, http.StatusBadRequest, "Ручная цена должна быть от 0 до 10 000 000 000 ₽.")
				return
			}
			unitPriceKopecks = *item.UnitPriceKopecks
		}
		lineSubtotal := unitPriceKopecks * item.Portions
		lineTotal := int64(math.Round(float64(lineSubtotal) * (1 - float64(item.ItemDiscountPercent)/100) * (1 - float64(input.GlobalDiscountPercent)/100)))
		if lineSubtotal < 0 || lineTotal < 0 {
			a.writeError(w, http.StatusBadRequest, "Не удалось рассчитать стоимость позиции")
			return
		}
		subtotalTotal += lineSubtotal
		grandTotal += lineTotal
		_, err = tx.Exec(
			"INSERT INTO customer_request_items(request_id,dish_id,dish_name,portions,unit_price_kopecks,item_discount_percent,subtotal_kopecks,total_kopecks) VALUES(?,?,?,?,?,?,?,?)",
			requestID, dishID, dishName, item.Portions, unitPriceKopecks, item.ItemDiscountPercent, lineSubtotal, lineTotal,
		)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить позицию заявки")
			return
		}
	}
	if _, err := tx.Exec("UPDATE customer_requests SET subtotal_kopecks=?,total_kopecks=? WHERE id=?", subtotalTotal, grandTotal, requestID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось сохранить итоги заявки")
		return
	}
	if err := tx.Commit(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			a.writeError(w, http.StatusConflict, "Заявка из этого файла уже сохранена")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "Не удалось завершить сохранение заявки")
		return
	}
	a.bumpRev()
	request, err := a.loadSavedRequest(requestID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Заявка сохранена, но не удалось загрузить её карточку")
		return
	}
	a.sendJSON(w, http.StatusCreated, request)
}

func (a *API) Requests(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query("SELECT id FROM customer_requests ORDER BY id DESC LIMIT 100")
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить историю заявок")
		return
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать историю заявок")
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.writeError(w, http.StatusInternalServerError, "Не удалось прочитать историю заявок")
		return
	}
	rows.Close()

	result := make([]savedRequest, 0, len(ids))
	for _, id := range ids {
		item, err := a.loadSavedRequest(id)
		if err != nil {
			a.writeError(w, http.StatusInternalServerError, "Не удалось открыть сохранённую заявку")
			return
		}
		result = append(result, item)
	}
	a.sendJSON(w, http.StatusOK, result)
}

func (a *API) RequestByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Некорректный номер заявки")
		return
	}
	item, err := a.loadSavedRequest(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.writeError(w, http.StatusNotFound, "Заявка не найдена")
		} else {
			a.writeError(w, http.StatusInternalServerError, "Не удалось открыть заявку")
		}
		return
	}
	a.sendJSON(w, http.StatusOK, item)
}

func (a *API) loadSavedRequest(id int64) (savedRequest, error) {
	var item savedRequest
	var created string
	var subtotal, total int64
	err := a.DB.QueryRow(
		"SELECT id,request_number,customer_name,source_filename,created_at,global_discount_percent,subtotal_kopecks,total_kopecks,status FROM customer_requests WHERE id=?",
		id,
	).Scan(&id, &item.Number, &item.CustomerName, &item.SourceFilename, &created, &item.GlobalDiscountPercent, &subtotal, &total, &item.Status)
	if err != nil {
		return item, err
	}
	item.ID = strconv.FormatInt(id, 10)
	item.CreatedAt = parseTime(created)
	item.SubtotalKopecks = subtotal
	item.TotalKopecks = total
	item.DiscountKopecks = subtotal - total
	rows, err := a.DB.Query(
		"SELECT dish_id,dish_name,portions,unit_price_kopecks,item_discount_percent,subtotal_kopecks,total_kopecks FROM customer_request_items WHERE request_id=? ORDER BY id",
		id,
	)
	if err != nil {
		return item, err
	}
	item.Items = make([]savedRequestItem, 0)
	for rows.Next() {
		var line savedRequestItem
		var dishID int64
		if err := rows.Scan(&dishID, &line.DishName, &line.Portions, &line.UnitPriceKopecks, &line.ItemDiscountPercent, &line.SubtotalKopecks, &line.TotalKopecks); err != nil {
			rows.Close()
			return item, err
		}
		line.DishID = strconv.FormatInt(dishID, 10)
		item.Items = append(item.Items, line)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return item, err
	}
	rows.Close()
	return item, nil
}
