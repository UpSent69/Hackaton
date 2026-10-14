package handlers

import (
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
	baseURL, model := requestimport.OllamaConfig()

	log.Printf(
		"Импорт поставки: файл=%s, размер=%d байт, текст=%d символов, изображение=%d байт, модель=%s",
		filename, len(data), len([]rune(source.Text)), len(source.Image), model,
	)

	warnings := make([]string, 0)
	var document requestimport.SupplyDocument
	aiUsed := false
	parserMode := "ai"

	// Simple text tables (including the CSV template) are read without waiting for the model.
	ext := strings.ToLower(filepath.Ext(filename))
	if source.Text != "" && (ext == ".txt" || ext == ".csv" || ext == ".tsv" || ext == ".md" || ext == ".log") {
		if candidate := requestimport.FallbackSupply(source.Text); len(candidate.Items) > 0 {
			document = candidate
			parserMode = "structured"
		}
	}

	if parserMode != "structured" {
		// nginx closes the proxied request after 60 s, so the model gets a little less.
		ctx, cancel := context.WithTimeout(r.Context(), 52*time.Second)
		started := time.Now()
		var aiErr error
		document, aiErr = requestimport.AnalyzeSupply(ctx, source, baseURL, model)
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
	defer requestMu.Unlock()

	products, err := a.requestCatalog()
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "Не удалось загрузить каталог продуктов")
		return
	}

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
		// Only an exact name match is used: a supply may create a new product, and
		// "Сливки 10 %" must not be merged into an existing "Сливки 33 %".
		productUnit := ""
		for _, product := range products {
			if normalizeRequestName(product.Name) == normalizeRequestName(raw.ProductName) {
				row.Name = product.Name
				row.CatalogMatch = true
				productUnit = product.Unit
				break
			}
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
	if len(items) == 0 {
		warnings = append(warnings, "Не удалось уверенно выделить позиции. Их можно добавить вручную.")
	}
	// POST /supplies accepts numbers up to 80 bytes.
	supplyNumber := truncateRunes(strings.TrimSpace(document.SupplyNumber), 40)
	if supplyNumber != "" {
		var exists bool
		if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM supplies WHERE number=?)", supplyNumber).Scan(&exists); err == nil && exists {
			warnings = append(warnings, fmt.Sprintf("Поставка с номером «%s» уже есть в истории. Для сохранения укажите другой номер.", supplyNumber))
		}
	}

	duplicate := false
	if err := a.DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM supply_imports WHERE source_sha256=? AND supply_id IS NOT NULL)", hashHex,
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
		"INSERT INTO supply_imports(source_filename,source_sha256,parser_mode,ai_used,detected_number,items_json,warnings_json,created_at) VALUES(?,?,?,?,?,?,?,?)",
		filename, hashHex, parserMode, aiFlag, supplyNumber, string(itemsJSON), string(warningsJSON), formatTime(now),
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
