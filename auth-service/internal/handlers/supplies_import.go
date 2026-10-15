package handlers

import (
	"auth-service/internal/contract"
	"auth-service/internal/requestimport"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Shelf life put into a row when the document does not state one (agreed range, hours).
const (
	randomShelfLifeMin = 50
	randomShelfLifeMax = 300
	randomLotLength    = 8
	lotAlphabet        = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

type supplyPreviewItem struct {
	OriginalName       string  `json:"originalName"`
	Name               string  `json:"name"`
	CatalogMatch       bool    `json:"catalogMatch"`
	Unit               string  `json:"unit"`
	Quantity           int64   `json:"quantity"`
	ShelfLifeHours     int64   `json:"shelfLifeHours"`
	ShelfLifeGenerated bool    `json:"shelfLifeGenerated"`
	LotNumber          string  `json:"lotNumber"`
	LotGenerated       bool    `json:"lotGenerated"`
	Confidence         float64 `json:"confidence"`
}

type parsedSupplyResponse struct {
	ImportID       string              `json:"importId"`
	SupplyNumber   string              `json:"supplyNumber"`
	SourceFilename string              `json:"sourceFilename"`
	SourceHash     string              `json:"sourceHash"`
	AIUsed         bool                `json:"aiUsed"`
	ParserMode     string              `json:"parserMode"`
	DuplicateSaved bool                `json:"duplicateSaved"`
	Items          []supplyPreviewItem `json:"items"`
	Warnings       []string            `json:"warnings"`
}

func randomInt(max int64) int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return time.Now().UnixNano() % max
	}
	return n.Int64()
}

// randomLotNumber is used when the document (or the form) gives no batch number.
func randomLotNumber() string {
	var b strings.Builder
	for i := 0; i < randomLotLength; i++ {
		b.WriteByte(lotAlphabet[randomInt(int64(len(lotAlphabet)))])
	}
	return b.String()
}

// truncateRunes keeps whole characters: Cyrillic text must not be cut in the middle of a rune.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func randomShelfLifeHours() int64 {
	return randomShelfLifeMin + randomInt(randomShelfLifeMax-randomShelfLifeMin+1)
}

// supplyQuantity converts the document unit into the product's base unit (g, ml, pcs).
// Unknown products take the base unit of the document, or grams like the manual form.
func supplyQuantity(quantity float64, docUnit, productUnit string) (int64, string, bool) {
	docUnit = requestimport.NormalizeUnit(docUnit)
	base := productUnit
	if base == "" {
		switch docUnit {
		case "kg", "g":
			base = "g"
		case "l", "ml":
			base = "ml"
		case "pcs":
			base = "pcs"
		default:
			base = "g"
		}
	}
	if docUnit == "" {
		return int64(math.Round(quantity)), base, true
	}
	converted, ok := requestimport.ConvertQuantity(quantity, docUnit, base)
	return int64(math.Round(converted)), base, ok
}

// ParseSupplyFile reads a delivery document in memory, extracts its rows with the local
// model (or the table parser) and logs the result in supply_imports. The supply itself
// is written only by POST /supplies after the user has checked the table.
func (a *API) ParseSupplyFile(w http.ResponseWriter, r *http.Request) {
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
		a.writeError(w, http.StatusBadRequest, "Выберите файл поставки")
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
		"Импорт поставки: файл=%s, размер=%d байт, текст=%d символов, изображение=%d байт",
		filename, len(data), len([]rune(source.Text)), len(source.Image),
	)

	// Простые таблицы (в том числе шаблон) разбираются сразу, всё остальное — через очередь Qwen.
	if isPlainTable(filename, source) {
		if document := requestimport.FallbackSupply(source.Text); len(document.Items) > 0 {
			names := make([]string, 0, len(document.Items))
			for _, item := range document.Items {
				names = append(names, item.ProductName)
			}
			if !needsModelMatch(names, a.productNames(restaurantOf(r))) {
				a.finishSupplyParse(r.Context(), w, restaurantOf(r), filename, hashHex, source)
				return
			}
		}
	}
	a.enqueueDocument(w, restaurantOf(r), "supply", filename, hashHex, source)
}

// isPlainTable — текстовый файл, который можно разобрать без модели.
func isPlainTable(filename string, source requestimport.Source) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return source.Text != "" && (ext == ".txt" || ext == ".csv" || ext == ".tsv" || ext == ".md" || ext == ".log")
}

// finishSupplyParse распознаёт накладную, сопоставляет строки с каталогом и пишет журнал
// supply_imports. Вызывается из запроса (простые таблицы) или из очереди документов.
func (a *API) finishSupplyParse(ctx context.Context, w http.ResponseWriter, restaurantID int64, filename, hashHex string, source requestimport.Source) {
	baseURL, model := requestimport.OllamaConfig()
	warnings := make([]string, 0)
	var document requestimport.SupplyDocument
	aiUsed := false
	parserMode := "ai"

	// Simple text tables (including the CSV template) are read without waiting for the model.
	if isPlainTable(filename, source) {
		if candidate := requestimport.FallbackSupply(source.Text); len(candidate.Items) > 0 {
			document = candidate
			parserMode = "structured"
		}
	}

	if parserMode != "structured" {
		// Распознавание идёт в очереди документов, поэтому модели даётся общий срок задачи.
		aiCtx, cancel := context.WithTimeout(ctx, jobTimeout)
		started := time.Now()
		var aiErr error
		document, aiErr = requestimport.AnalyzeSupply(aiCtx, source, baseURL, model)
		cancel()
		log.Printf("Анализ поставки Ollama завершён: время=%s, ошибка=%v", time.Since(started), aiErr)

		if aiErr != nil {
			document = requestimport.FallbackSupply(source.Text)
			parserMode = "fallback"
			warnings = append(warnings, "Нейросеть не ответила — включён упрощённый разбор. Проверьте каждую строку.")
		} else if len(document.Items) == 0 && source.Text != "" {
			candidate := requestimport.FallbackSupply(source.Text)
			parserMode = "fallback"
			if len(candidate.Items) > 0 {
				candidate.SupplyNumber = document.SupplyNumber
				document = candidate
				warnings = append(warnings, "Нейросеть не выделила позиции; строки восстановлены упрощённым разбором. Проверьте распознавание.")
			}
		} else if len(document.Items) > 0 {
			aiUsed = true
		}
	}

	requestMu.Lock()
	products, err := a.requestCatalog(restaurantID)
	requestMu.Unlock()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить каталог продуктов")
		return
	}
	// Позиции сопоставляются с продуктами склада по смыслу («Помидоры свежие» → «Томаты»).
	// Если склад пустой (первая поставка), всё заводится как новое; иначе то, чему нет пары
	// на складе, в поставку не попадает — об этом сообщается отдельно.
	names := make([]string, 0, len(document.Items))
	for _, item := range document.Items {
		names = append(names, item.ProductName)
	}
	catalog := make([]string, len(products))
	for i, product := range products {
		catalog[i] = product.Name
	}
	matches := a.matchNames(ctx, names, catalog, "product")
	skipped := make(map[string]bool)

	requestMu.Lock()
	defer requestMu.Unlock()

	items := make([]supplyPreviewItem, 0, len(document.Items))
	// One supply cannot hold the same product twice, so repeated rows are merged.
	byName := make(map[string]int)
	generatedShelf := 0
	for _, raw := range document.Items {
		row := supplyPreviewItem{
			OriginalName: strings.TrimSpace(raw.ProductName),
			Name:         strings.TrimSpace(raw.ProductName),
			Confidence:   raw.Confidence,
		}
		productUnit := ""
		if index, ok := matches[raw.ProductName]; ok {
			row.Name = products[index].Name
			row.CatalogMatch = true
			productUnit = products[index].Unit
		} else if len(products) > 0 {
			skipped[row.OriginalName] = true
			continue
		}
		quantity, unit, convertible := supplyQuantity(raw.Quantity, raw.Unit, productUnit)
		if !convertible {
			warnings = append(warnings, fmt.Sprintf("У продукта «%s» в документе единица «%s», а на складе он учитывается в «%s». Проверьте количество.", row.Name, raw.Unit, productUnit))
		}
		if quantity <= 0 {
			warnings = append(warnings, fmt.Sprintf("Строка «%s» пропущена: количество меньше единицы.", row.OriginalName))
			continue
		}
		row.Quantity = quantity
		row.Unit = unit

		row.ShelfLifeHours = int64(math.Round(raw.ShelfLifeHours))
		if row.ShelfLifeHours <= 0 {
			row.ShelfLifeHours = randomShelfLifeHours()
			row.ShelfLifeGenerated = true
			generatedShelf++
		}
		// POST /supplies accepts lot numbers up to 40 bytes.
		row.LotNumber = truncateRunes(strings.TrimSpace(raw.LotNumber), 20)
		if row.LotNumber == "" {
			row.LotNumber = randomLotNumber()
			row.LotGenerated = true
		}

		key := normalizeName(row.Name)
		if index, seen := byName[key]; seen && items[index].Unit == row.Unit {
			items[index].Quantity += row.Quantity
			warnings = append(warnings, fmt.Sprintf("Продукт «%s» встретился в документе несколько раз — количества сложены в одну строку.", row.Name))
			continue
		}
		byName[key] = len(items)
		items = append(items, row)
	}

	if generatedShelf > 0 {
		warnings = append(warnings, fmt.Sprintf("Срок хранения не указан в документе для позиций: %d. Подставлен случайный срок от %d до %d ч — проверьте его.", generatedShelf, randomShelfLifeMin, randomShelfLifeMax))
	}
	if len(skipped) > 0 {
		warnings = append(warnings, fmt.Sprintf("Не добавлено — этих продуктов нет на складе (%d из %d): %s. Их можно добавить вручную строкой в таблице.",
			len(skipped), len(document.Items), strings.Join(requestimport.SortedKeys(skipped), ", ")))
	}
	if len(items) == 0 && len(skipped) == 0 {
		warnings = append(warnings, "Не удалось уверенно выделить позиции. Их можно добавить вручную.")
	}
	// POST /supplies accepts numbers up to 80 bytes.
	supplyNumber := truncateRunes(strings.TrimSpace(document.SupplyNumber), 40)
	if supplyNumber != "" {
		var exists bool
		if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM supplies WHERE number=? AND restaurant_id=?)", supplyNumber, restaurantID).Scan(&exists); err == nil && exists {
			warnings = append(warnings, fmt.Sprintf("Поставка с номером «%s» уже есть в истории. Для сохранения укажите другой номер.", supplyNumber))
		}
	}

	duplicate := false
	if err := a.DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM supply_imports WHERE source_sha256=? AND supply_id IS NOT NULL AND restaurant_id=?)", hashHex, restaurantID,
	).Scan(&duplicate); err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось проверить повторную загрузку")
		return
	}
	if duplicate {
		warnings = append(warnings, "Этот файл уже был принят как поставка. Проверьте, не задвоится ли склад.")
	}
	warnings = uniqueStrings(warnings)

	itemsJSON, _ := json.Marshal(items)
	warningsJSON, _ := json.Marshal(warnings)
	aiFlag := 0
	if aiUsed {
		aiFlag = 1
	}
	now, _ := a.nowAndSpeed()
	result, err := a.DB.Exec(
		"INSERT INTO supply_imports(source_filename,source_sha256,parser_mode,ai_used,detected_number,items_json,warnings_json,created_at,restaurant_id) VALUES(?,?,?,?,?,?,?,?,?)",
		filename, hashHex, parserMode, aiFlag, supplyNumber, string(itemsJSON), string(warningsJSON), formatTime(now), restaurantID,
	)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось записать журнал импорта поставки")
		return
	}
	importID, err := result.LastInsertId()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось записать журнал импорта поставки")
		return
	}
	log.Printf("Импорт поставки завершён: режим=%s, позиций=%d, журнал=%d", parserMode, len(items), importID)

	a.sendJSON(w, http.StatusOK, parsedSupplyResponse{
		ImportID: strconv.FormatInt(importID, 10), SupplyNumber: supplyNumber,
		SourceFilename: filename, SourceHash: hashHex, AIUsed: aiUsed, ParserMode: parserMode,
		DuplicateSaved: duplicate, Items: items, Warnings: warnings,
	})
}

// matchNames сопоставляет названия из документа со списком (продукты склада или блюда меню):
// сначала правила (синонимы, формы слов, «свежие» и жирность не важны), затем Qwen для
// оставшихся с повторной проверкой каждой пары. Возвращает индекс в catalog.
func (a *API) matchNames(ctx context.Context, names, catalog []string, kind string) map[string]int {
	result := make(map[string]int)
	if len(catalog) == 0 {
		return result
	}
	index := make(map[string]int, len(catalog))
	for i, name := range catalog {
		index[name] = i
	}
	unmatched := make([]string, 0)
	asked := make(map[string]bool)
	for _, name := range names {
		if i, ok := requestimport.MatchByRules(name, catalog); ok {
			result[name] = i
		} else if !asked[name] {
			asked[name] = true
			unmatched = append(unmatched, name)
		}
	}
	if len(unmatched) == 0 {
		return result
	}
	baseURL, model := requestimport.OllamaConfig()
	modelCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	found, err := requestimport.MatchWithModel(modelCtx, unmatched, catalog, kind, baseURL, model)
	log.Printf("Сопоставление (%s): правилами %d, Qwen из %d нашла %d, ошибка=%v", kind, len(result), len(unmatched), len(found), err)
	for name, match := range found {
		result[name] = index[match]
	}
	return result
}

// needsModelMatch — есть ли названия, для которых правила не нашли пару (тогда документ
// идёт в очередь: ответ Qwen может занять дольше, чем держит nginx).
func needsModelMatch(names, catalog []string) bool {
	if len(catalog) == 0 {
		return false
	}
	for _, name := range names {
		if _, ok := requestimport.MatchByRules(name, catalog); !ok {
			return true
		}
	}
	return false
}

// productNames — названия продуктов склада ресторана.
func (a *API) productNames(restaurantID int64) []string {
	requestMu.Lock()
	products, err := a.requestCatalog(restaurantID)
	requestMu.Unlock()
	if err != nil {
		return nil
	}
	names := make([]string, len(products))
	for i, product := range products {
		names[i] = product.Name
	}
	return names
}

// menuDishes — блюда меню с ценой порции и числом порций, которые можно приготовить сейчас.
func (a *API) menuDishes(restaurantID int64) ([]contract.Dish, error) {
	requestMu.Lock()
	defer requestMu.Unlock()
	now, _ := a.nowAndSpeed()
	rows, err := a.DB.Query("SELECT id FROM dishes WHERE restaurant_id=? ORDER BY name COLLATE NOCASE,id", restaurantID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	dishes := make([]contract.Dish, 0, len(ids))
	for _, id := range ids {
		dish, err := a.buildDish(id, now)
		if err != nil {
			return nil, err
		}
		dishes = append(dishes, dish)
	}
	return dishes, nil
}
