package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_IndexesAndParsesWindow(t *testing.T) {
	path := writeTemp(t, `
version: "1.0"
default_action: ALLOW
agents:
  - id: "agent-1"
    policies:
      - provider: "openai"
        action: "BLOCK"
        limit: 50.0
        window: "1h"
`)
	idx, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if idx.DefaultAction != ActionAllow {
		t.Fatalf("default action = %q, want ALLOW", idx.DefaultAction)
	}
	pols, ok := idx.LookupPolicies("agent-1", "openai")
	if !ok || len(pols) != 1 {
		t.Fatalf("lookup = %v, ok=%v", pols, ok)
	}
	if pols[0].WindowDuration != time.Hour {
		t.Fatalf("window = %v, want 1h", pols[0].WindowDuration)
	}
}

func TestLoad_DefaultsToBlock(t *testing.T) {
	path := writeTemp(t, `
version: "1.0"
agents: []
`)
	idx, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if idx.DefaultAction != ActionBlock {
		t.Fatalf("default action = %q, want BLOCK", idx.DefaultAction)
	}
}

func TestLoad_RejectsBadWindow(t *testing.T) {
	path := writeTemp(t, `
version: "1.0"
agents:
  - id: "a"
    policies:
      - provider: "openai"
        action: "BLOCK"
        limit: 1.0
        window: "banana"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid window")
	}
}

func TestLookup_UnknownAgent(t *testing.T) {
	path := writeTemp(t, `version: "1.0"
agents: []`)
	idx, _ := Load(path)
	if _, ok := idx.LookupPolicies("ghost", "openai"); ok {
		t.Fatal("unknown agent should report not found")
	}
}
