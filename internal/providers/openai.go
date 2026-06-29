package providers

import (
	"encoding/json"
	"net/http"
)

// OpenAI pricing in USD per 1M tokens (input / output).
// Update this map as OpenAI changes pricing.
var openAIPricing = map[string][2]float64{
	"gpt-4o":             {2.50, 10.00},
	"gpt-4o-mini":        {0.15, 0.60},
	"gpt-4-turbo":        {10.00, 30.00},
	"gpt-4":              {30.00, 60.00},
	"gpt-3.5-turbo":      {0.50, 1.50},
	"o1":                 {15.00, 60.00},
	"o1-mini":            {3.00, 12.00},
	"o1-pro":             {150.00, 600.00},
	"o3-mini":            {1.10, 4.40},
	"_default":           {10.00, 30.00}, // conservative fallback
}

type OpenAI struct{}

func (o *OpenAI) Name() string        { return "openai" }
func (o *OpenAI) UpstreamBase() string { return "https://api.openai.com" }

func (o *OpenAI) EstimateCost(r *http.Request, body []byte) (float64, error) {
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		// Non-chat endpoint or unparseable — apply a conservative flat estimate.
		return 0.01, nil
	}

	pricing, ok := openAIPricing[req.Model]
	if !ok {
		pricing = openAIPricing["_default"]
	}

	inputChars := 0
	for _, m := range req.Messages {
		switch v := m.Content.(type) {
		case string:
			inputChars += len(v)
		case []any:
			// multi-part content blocks
			for _, part := range v {
				if pm, ok := part.(map[string]any); ok {
					if text, ok := pm["text"].(string); ok {
						inputChars += len(text)
					}
				}
			}
		}
	}

	// Conservative: 3 chars per token (real ratio is ~4, so this overestimates).
	inputTokens := float64(inputChars) / 3.0

	outputTokens := float64(req.MaxTokens)
	if outputTokens == 0 {
		// Assume up to 1024 output tokens when not specified.
		outputTokens = 1024
	}

	inputCost := inputTokens / 1_000_000 * pricing[0]
	outputCost := outputTokens / 1_000_000 * pricing[1]
	return inputCost + outputCost, nil
}

func (o *OpenAI) ExtractActualCost(resp *http.Response, body []byte) (float64, error) {
	var res struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Usage == nil {
		return 0, nil
	}

	pricing, ok := openAIPricing[res.Model]
	if !ok {
		pricing = openAIPricing["_default"]
	}

	inputCost := float64(res.Usage.PromptTokens) / 1_000_000 * pricing[0]
	outputCost := float64(res.Usage.CompletionTokens) / 1_000_000 * pricing[1]
	return inputCost + outputCost, nil
}
