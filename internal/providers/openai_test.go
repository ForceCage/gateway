package providers

import (
	"net/http"
	"strings"
	"testing"
)

func TestOpenAI_EstimateCost(t *testing.T) {
	o := &OpenAI{}
	body := []byte(`{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"hello world"}]}`)
	cost, err := o.EstimateCost(&http.Request{}, body)
	if err != nil {
		t.Fatal(err)
	}
	if cost <= 0 {
		t.Fatalf("expected positive cost, got %v", cost)
	}
}

func TestOpenAI_EstimateUnknownModelUsesDefault(t *testing.T) {
	o := &OpenAI{}
	body := []byte(`{"model":"some-future-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	cost, err := o.EstimateCost(&http.Request{}, body)
	if err != nil || cost <= 0 {
		t.Fatalf("default pricing should yield positive cost: cost=%v err=%v", cost, err)
	}
}

func TestOpenAI_ExtractActualCost(t *testing.T) {
	o := &OpenAI{}
	resp := &http.Response{}
	body := []byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}`)
	cost, err := o.ExtractActualCost(resp, body)
	if err != nil {
		t.Fatal(err)
	}
	// 1M input @ $2.50 + 1M output @ $10.00 = $12.50
	if cost < 12.49 || cost > 12.51 {
		t.Fatalf("cost = %v, want ~12.50", cost)
	}
}

func TestOpenAI_ExtractActualCost_NoUsage(t *testing.T) {
	o := &OpenAI{}
	cost, err := o.ExtractActualCost(&http.Response{}, []byte(`{"model":"gpt-4o"}`))
	if err != nil || cost != 0 {
		t.Fatalf("missing usage should give 0 cost, no error: cost=%v err=%v", cost, err)
	}
}

func TestOpenAI_EstimateConservativeUnparseable(t *testing.T) {
	o := &OpenAI{}
	cost, err := o.ExtractActualCost(&http.Response{}, []byte(strings.Repeat("x", 10)))
	if err != nil || cost != 0 {
		t.Fatalf("unparseable body should give 0 cost, no error: cost=%v err=%v", cost, err)
	}
}
