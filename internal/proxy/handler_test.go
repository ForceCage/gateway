package proxy

import (
	"testing"

	"github.com/forcecage/gateway/internal/config"
)

func TestModelFromBody(t *testing.T) {
	if got := modelFromBody([]byte(`{"model":"gpt-4o"}`)); got != "gpt-4o" {
		t.Fatalf("got %q", got)
	}
	if got := modelFromBody([]byte(`not json`)); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestMatchingPolicies(t *testing.T) {
	scoped := &config.Policy{Provider: "openai", Models: []string{"gpt-4o", "o1-pro"}}
	global := &config.Policy{Provider: "openai"} // no Models → applies to all
	pols := []*config.Policy{scoped, global}

	// Request for gpt-4o → both apply.
	got := matchingPolicies(pols, "gpt-4o")
	if len(got) != 2 {
		t.Fatalf("gpt-4o: got %d policies, want 2", len(got))
	}

	// Request for an unlisted model → only the global policy applies.
	got = matchingPolicies(pols, "gpt-3.5-turbo")
	if len(got) != 1 || got[0] != global {
		t.Fatalf("gpt-3.5-turbo: got %v, want [global]", got)
	}

	// No model specified → only the global policy applies.
	got = matchingPolicies(pols, "")
	if len(got) != 1 || got[0] != global {
		t.Fatalf("empty model: got %v, want [global]", got)
	}
}
