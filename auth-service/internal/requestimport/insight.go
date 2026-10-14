package requestimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Insight is the model's reading of the warehouse statistics shown on the analytics page.
type Insight struct {
	Summary string   `json:"summary"`
	Risks   []string `json:"risks"`
	Actions []string `json:"actions"`
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

	prompt := "Ниже статистика склада ресторана. Проанализируй её и верни только JSON с полями summary (строка, 2–3 предложения: общее состояние склада и продаж), risks (массив до 5 строк: главные проблемы — что испортится, что закончится, где отказы и списания), actions (массив до 5 строк: конкретные действия — что заказать, что продвигать, что убрать из меню). Опирайся только на приведённые данные, называй продукты и блюда и используй числа из данных, ничего не придумывай. Если данных мало, так и напиши. Пиши по-русски, коротко и по делу.\n\nДанные:\n" + facts

	body, err := json.Marshal(ollamaRequest{
		Model: model,
		Messages: []ollamaMessage{
			{Role: "system", Content: "Ты аналитик склада ресторана. Отвечай только валидным JSON."},
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Format:  "json",
		Think:   false,
		Options: map[string]any{"temperature": 0.2, "num_predict": 900},
	})
	if err != nil {
		return Insight{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Insight{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(req)
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
	if insight.Summary == "" && len(insight.Risks) == 0 && len(insight.Actions) == 0 {
		return Insight{}, errors.New("нейросеть вернула пустой анализ")
	}
	return insight, nil
}

func cleanLines(lines []string, limit int) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && len(result) < limit {
			result = append(result, line)
		}
	}
	return result
}
