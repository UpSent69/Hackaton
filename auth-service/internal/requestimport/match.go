package requestimport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode"
)

// Сопоставление названия из документа с продуктами склада «по смыслу»: «Помидоры свежие» —
// это «Томаты», «Огурцы свежие» — «Огурцы», «Яйца куриные» — «Яйца». Сначала быстрые правила,
// затем, для того что правила не нашли, — Qwen со списком продуктов склада.

// Слова, которые описывают состояние или сорт, а не сам продукт.
var productNoise = []string{
	"свеж", "охла", "замо", "отбо", "высш", "сорт", "весо", "фасо", "упак", "импо", "мест", "домаш", "нату", "крупн", "мелк",
}

// Синонимы на уровне основы слова: левая основа заменяется правой.
var productSynonyms = map[string]string{
	"поми": "тома", // помидоры = томаты
	"груд": "фил",  // куриная грудка = куриное филе
	"карт": "карт", // картофель, картошка
	"говя": "говя", // говяжий, говядина
	"буря": "свек", // буряк = свёкла
	"цуки": "каба", // цукини = кабачки
	"шамп": "гриб", // шампиньоны = грибы
}

func productStem(word string) string {
	runes := []rune(word)
	if len(runes) >= 5 {
		return string(runes[:4])
	}
	// Короткие слова: отбрасываем окончание, чтобы «яйца» и «яйцо» совпали.
	if len(runes) > 3 && strings.ContainsRune("аяоеиыуюь", runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// productTokens — набор основ значимых слов названия.
func productTokens(name string) map[string]bool {
	name = strings.ToLower(strings.ReplaceAll(name, "ё", "е"))
	words := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) })
	tokens := make(map[string]bool)
	for _, word := range words {
		if len([]rune(word)) < 3 {
			continue // предлоги, «кг», «г», «л», «шт»
		}
		stem := productStem(word)
		noise := false
		for _, prefix := range productNoise {
			if strings.HasPrefix(word, prefix) {
				noise = true
				break
			}
		}
		if noise || stem == "кг" || stem == "шт" {
			continue
		}
		if synonym, ok := productSynonyms[stem]; ok {
			stem = synonym
		}
		tokens[stem] = true
	}
	return tokens
}

func sameTokens(a, b map[string]bool) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for token := range a {
		if !b[token] {
			return false
		}
	}
	return true
}

func subsetTokens(small, big map[string]bool) bool {
	if len(small) == 0 {
		return false
	}
	for token := range small {
		if !big[token] {
			return false
		}
	}
	return true
}

// MatchByRules ищет продукт каталога для названия из документа. Возвращает индекс в catalog.
// Совпадение засчитывается, если основы слов совпадают полностью или название из каталога
// целиком входит в название из документа («Сыр» — в «Сыр твёрдый») и такой продукт один.
func MatchByRules(name string, catalog []string) (int, bool) {
	wanted := productTokens(name)
	if len(wanted) == 0 {
		return -1, false
	}
	exact, partial, partialSize, partialCount := -1, -1, 0, 0
	for index, candidate := range catalog {
		tokens := productTokens(candidate)
		if sameTokens(wanted, tokens) {
			if exact >= 0 {
				return -1, false // два одинаковых продукта в каталоге — не угадываем
			}
			exact = index
			continue
		}
		if subsetTokens(tokens, wanted) {
			switch {
			case len(tokens) > partialSize:
				partial, partialSize, partialCount = index, len(tokens), 1
			case len(tokens) == partialSize:
				partialCount++
			}
		}
	}
	if exact >= 0 {
		return exact, true
	}
	if partialCount == 1 {
		return partial, true
	}
	return -1, false
}

// MatchWithModel спрашивает Qwen, какому продукту склада соответствует каждое название.
// Принимаются только ответы, которые дословно есть в каталоге; остальное — «нет на складе».
// kind — "product" (продукты склада) или "dish" (блюда меню): от него зависят формулировки и примеры.
func MatchWithModel(ctx context.Context, names, catalog []string, kind, baseURL, model string) (map[string]string, error) {
	result := make(map[string]string)
	if len(names) == 0 || len(catalog) == 0 {
		return result, nil
	}
	if model == "" {
		model = "qwen3.5:2b"
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	prompt := "Каталог продуктов склада ресторана:\n- " + strings.Join(catalog, "\n- ") +
		"\n\nДля каждой позиции из документа найди продукт каталога, который означает ТО ЖЕ САМОЕ (синонимы, другое название, уточнения вроде «свежие», «куриные», жирность или сорт не важны: «Помидоры свежие» = «Томаты», «Куриная грудка» = «Куриное филе»). " +
		"Если в каталоге нет такого продукта, верни пустую строку — не подбирай похожий другой продукт (бананы — не яблоки, молоко — не сливки). "
	if kind == "dish" {
		prompt = "Меню ресторана:\n- " + strings.Join(catalog, "\n- ") +
			"\n\nДля каждого блюда из заявки найди блюдо меню, которое означает ТО ЖЕ САМОЕ (другое название, уточнение подачи или порядок слов не важны: «Салат из помидоров» = «Салат с томатами», «Сливочный суп» = «Суп со сливками»). " +
			"Если в меню нет такого блюда, верни пустую строку — не подбирай другое блюдо (карбонара — не паста с курицей, если в ней нет курицы; борщ — не суп со сливками). "
	}
	prompt += 
		"Верни только JSON вида {\"matches\":[{\"item\":\"позиция из документа\",\"product\":\"точное название из каталога или пустая строка\"}]}.\n\nПозиции документа:\n- " + strings.Join(names, "\n- ")

	body, err := json.Marshal(ollamaRequest{
		Model: model,
		Messages: []ollamaMessage{
			{Role: "system", Content: "Ты сопоставляешь названия продуктов с каталогом склада. Отвечай только валидным JSON."},
			{Role: "user", Content: prompt},
		},
		Stream: false, Format: "json", Think: false,
		Options:   map[string]any{"temperature": 0, "num_predict": 900},
		KeepAlive: ollamaKeepAlive,
	})
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result, fmt.Errorf("Ollama недоступен: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return result, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("Ollama вернул HTTP %d", resp.StatusCode)
	}
	var response ollamaResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return result, err
	}
	var parsed struct {
		Matches []struct {
			Item    string `json:"item"`
			Product string `json:"product"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(response.Message.Content), &parsed); err != nil {
		return result, fmt.Errorf("нейросеть вернула некорректный JSON: %w", err)
	}
	known := make(map[string]string, len(catalog))
	for _, name := range catalog {
		known[strings.ToLower(strings.TrimSpace(name))] = name
	}
	asked := make(map[string]string, len(names))
	for _, name := range names {
		asked[strings.ToLower(strings.TrimSpace(name))] = name
	}
	for _, match := range parsed.Matches {
		item, okItem := asked[strings.ToLower(strings.TrimSpace(match.Item))]
		product, okProduct := known[strings.ToLower(strings.TrimSpace(match.Product))]
		// Маленькая модель при выборе из списка ошибается (молоко → сливки), поэтому каждая
		// пара перепроверяется отдельным вопросом «да/нет»; принимается только подтверждённая.
		if okItem && okProduct && confirmSameProduct(ctx, item, product, kind, baseURL, model) {
			result[item] = product
		}
	}
	return result, nil
}

// confirmSameProduct — отдельный вопрос модели: можно ли записать позицию как этот продукт.
func confirmSameProduct(ctx context.Context, item, product, kind, baseURL, model string) bool {
	prompt := fmt.Sprintf("Нужно понять, можно ли записать позицию накладной «%s» на складе как «%s». "+
		"Да — если это тот же продукт под другим названием или с уточнением (свежие, сорт, жирность). Нет — если это другой продукт. "+
		"Примеры: Помидоры→Томаты: да. Куриная грудка→Куриное филе: да. Молоко→Сливки: нет. Бананы→Яблоки: нет. "+
		"Ответь JSON {\"answer\": \"да\"|\"нет\"}.", item, product)
	if kind == "dish" {
		prompt = fmt.Sprintf("Нужно понять, можно ли приготовить блюдо из заявки «%s» как блюдо меню «%s». "+
			"Да — если это то же блюдо под другим названием или с уточнением подачи. Нет — если это другое блюдо. "+
			"Примеры: Салат из помидоров→Салат с томатами: да. Сливочный суп→Суп со сливками: да. Борщ→Суп со сливками: нет. Паста карбонара→Паста с курицей: нет. "+
			"Ответь JSON {\"answer\": \"да\"|\"нет\"}.", item, product)
	}
	body, err := json.Marshal(ollamaRequest{
		Model: model, Messages: []ollamaMessage{{Role: "user", Content: prompt}},
		Stream: false, Format: "json", Think: false,
		Options: map[string]any{"temperature": 0, "num_predict": 20}, KeepAlive: ollamaKeepAlive,
	})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var response ollamaResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&response) != nil {
		return false
	}
	var answer struct {
		Answer string `json:"answer"`
	}
	if json.Unmarshal([]byte(response.Message.Content), &answer) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(answer.Answer), "да")
}

// SortedKeys — для стабильных сообщений.
func SortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
