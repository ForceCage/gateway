package providers

import "testing"

func TestGetByHost(t *testing.T) {
	r := NewRegistry()

	p, err := r.GetByHost("api.openai.com:443")
	if err != nil {
		t.Fatalf("openai host: %v", err)
	}
	if p.Name() != "openai" {
		t.Fatalf("got %q, want openai", p.Name())
	}

	p, err = r.GetByHost("api.anthropic.com")
	if err != nil || p.Name() != "anthropic" {
		t.Fatalf("anthropic host: p=%v err=%v", p, err)
	}

	if _, err := r.GetByHost("example.com"); err == nil {
		t.Fatal("expected error for unknown host")
	}
}
