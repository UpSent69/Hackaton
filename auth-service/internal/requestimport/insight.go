package requestimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Insight is the model's reading of the warehouse statistics shown on the analytics page.
type Insight struct {
	Summary   string    `json:"summary"`
	Risks     lineList  `json:"risks"`
	Actions   lineList  `json:"actions"`
	Season    string    `json:"season"`     // сезон и ближайшие праздники одной фразой
	MenuPicks lineList  `json:"menu_picks"` // блюда из меню, которые выгодно продвигать сейчас
	NewDishes lineList  `json:"new_dishes"` // сезонные блюда, которых нет в меню
}

// lineList — список строк, терпимый к формату маленькой модели: вместо строки она иногда
// присылает объект ({"блюдо": …, "есть": …}) или число. Объект склеивается в одну строку.
type lineList []string

func (l *lineList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		var single string
		if json.Unmarshal(data, &single) == nil {
			*l = lineList{single}
			return nil
		}
		return err
	}
	result := make(lineList, 0, len(raw))
	for _, item := range raw {
		if text := flattenJSON(item); text != "" {
			result = append(result, text)
		}
	}
	*l = result
	return nil
}

func flattenJSON(item json.RawMessage) string {
	var text string
	if json.Unmarshal(item, &text) == nil {
		return strings.TrimSpace(text)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(item, &object) == nil {
		// Порядок полей в map не сохраняется, поэтому название блюда, если оно есть, идёт первым.
		parts := make([]string, 0, len(object))
		for _, key := range []string{"name", "dish", "название", "блюдо", "title"} {
			if value, ok := object[key]; ok {
				parts = append(parts, flattenJSON(value))
				delete(object, key)
			}
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if value := flattenJSON(object[key]); value != "" {
				parts = append(parts, key+": "+value)
			}
		}
		return strings.Join(parts, " — ")
	}
	var list []json.RawMessage
	if json.Unmarshal(item, &list) == nil {
		parts := make([]string, 0, len(list))
		for _, value := range list {
			if text := flattenJSON(value); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, ", ")
	}
	return strings.Trim(strings.TrimSpace(string(item)), `"`)
}

// AnalyzeStats asks the same local Ollama model to interpret prepared statistics.
// The facts are already formatted in Russian by the server; the model only draws conclusions.
func AnalyzeStats(ctx context.Context, facts string, baseURL, model string) (Insight, error) {
	if model == "" {
		model = "qwen3.5:2b"
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	prompt := "Ниже статистика склада ресторана. Проанализируй её и верни только JSON с полями: " +
		"summary (строка, 2–3 предложения: общее состояние склада и продаж); " +
		"risks (массив до 5 строк: главные проблемы — что испортится, что закончится, где отказы и списания); " +
		"actions (массив до 5 строк: конкретные действия — что заказать, что продвигать, что убрать из меню); " +
		"season (строка: текущий сезон и ближайший праздник, если он есть в данных); " +
		"menu_picks (массив до 5 строк: блюда ТОЛЬКО из раздела «Меню», которые выгоднее всего продвигать сейчас — каждая строка начинается с точного названия блюда из меню, дальше коротко почему: подходит ли сезону или празднику, как продаётся, хватает ли порций, помогает ли израсходовать продукты, у которых скоро истекает срок); " +
		"new_dishes (массив до 4 строк: блюда, которых НЕТ в меню и которые подходят ТОЛЬКО текущему сезону и ближайшим праздникам — выбирай из строки «Сезонные блюда» в данных; не предлагай блюда другого сезона; каждая строка — название блюда, затем какие продукты для него уже есть на складе и каких не хватает). " +
		"Опирайся на приведённые данные, называй продукты и блюда и используй числа из данных, не придумывай продажи и остатки. Если данных мало, так и напиши. Пиши по-русски, коротко и по делу.\n\nДанные:\n" + facts

	body, err := json.Marshal(ollamaRequest{
		Model: model,
		Messages: []ollamaMessage{
			{Role: "system", Content: "Ты аналитик склада ресторана. Отвечай только валидным JSON."},
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Format:  "json",
		Think:   false,
		Options:   map[string]any{"temperature": 0.2, "num_predict": 1400},
		KeepAlive: ollamaKeepAlive,
	})
	if err != nil {
		return Insight{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Insight{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Insight{}, fmt.Errorf("Ollama недоступен: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return Insight{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure ollamaResponse
		_ = json.Unmarshal(raw, &failure)
		if failure.Error == "" {
			failure.Error = strings.TrimSpace(string(raw))
		}
		return Insight{}, fmt.Errorf("Ollama вернул HTTP %d: %s", resp.StatusCode, failure.Error)
	}
	var response ollamaResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return Insight{}, fmt.Errorf("не удалось разобрать ответ Ollama: %w", err)
	}
	var insight Insight
	if err := json.Unmarshal([]byte(response.Message.Content), &insight); err != nil {
		return Insight{}, fmt.Errorf("нейросеть вернула некорректный JSON: %w", err)
	}
	insight.Summary = strings.TrimSpace(insight.Summary)
	insight.Risks = cleanLines(insight.Risks, 5)
	insight.Actions = cleanLines(insight.Actions, 5)
	insight.Season = strings.TrimSpace(insight.Season)
	insight.MenuPicks = cleanLines(insight.MenuPicks, 5)
	insight.NewDishes = cleanLines(insight.NewDishes, 4)
	if insight.Summary == "" && len(insight.Risks) == 0 && len(insight.Actions) == 0 {
		return Insight{}, errors.New("нейросеть вернула пустой анализ")
	}
	return insight, nil
}

func cleanLines(lines lineList, limit int) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && len(result) < limit {
			result = append(result, line)
		}
	}
	return result
}
