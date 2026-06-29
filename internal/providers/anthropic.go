package providers

import (
	"encoding/json"
	"net/http"
)

// Anthropic pricing in USD per 1M tokens (input / output).
var anthropicPricing = map[string][2]float64{
	"claude-opus-4-8":            {15.00, 75.00},
	"claude-sonnet-4-6":          {3.00, 15.00},
	"claude-haiku-4-5-20251001":  {0.80, 4.00},
	"claude-3-5-sonnet-20241022": {3.00, 15.00},
	"claude-3-5-haiku-20241022":  {0.80, 4.00},
	"claude-3-opus-20240229":     {15.00, 75.00},
	"claude-3-haiku-20240307":    {0.25, 1.25},
	"_default":                   {15.00, 75.00},
}

type Anthropic struct{}

func (a *Anthropic) Name() string         { return "anthropic" }
func (a *Anthropic) UpstreamBase() string { return "https://api.anthropic.com" }

func (a *Anthropic) EstimateCost(r *http.Request, body []byte) (float64, error) {
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Content any `json:"content"`
		} `json:"messages"`
		System string `json:"system"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0.01, nil
	}

	pricing, ok := anthropicPricing[req.Model]
	if !ok {
		pricing = anthropicPricing["_default"]
	}

	inputChars := len(req.System)
	for _, m := range req.Messages {
		switch v := m.Content.(type) {
		case string:
			inputChars += len(v)
		case []any:
			for _, part := range v {
				if pm, ok := part.(map[string]any); ok {
					if text, ok := pm["text"].(string); ok {
						inputChars += len(text)
					}
				}
			}
		}
	}

	inputTokens := float64(inputChars) / 3.0

	outputTokens := float64(req.MaxTokens)
	if outputTokens == 0 {
		outputTokens = 1024
	}

	inputCost := inputTokens / 1_000_000 * pricing[0]
	outputCost := outputTokens / 1_000_000 * pricing[1]
	return inputCost + outputCost, nil
}

func (a *Anthropic) ExtractActualCost(resp *http.Response, body []byte) (float64, error) {
	var res struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Usage == nil {
		return 0, nil
	}

	pricing, ok := anthropicPricing[res.Model]
	if !ok {
		pricing = anthropicPricing["_default"]
	}

	inputCost := float64(res.Usage.InputTokens) / 1_000_000 * pricing[0]
	outputCost := float64(res.Usage.OutputTokens) / 1_000_000 * pricing[1]
	return inputCost + outputCost, nil
}
