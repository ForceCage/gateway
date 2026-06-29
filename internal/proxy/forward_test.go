package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/forcecage/gateway/internal/config"
	"github.com/forcecage/gateway/internal/providers"
)

func testForwardProxy(t *testing.T, idx *config.Index) *ForwardProxy {
	t.Helper()
	reg := providers.NewRegistry()
	ca, _, _, err := GenerateCertAuthority("Test CA")
	if err != nil {
		t.Fatal(err)
	}
	// tracker is nil: the deny paths under test return before any Redis call.
	engine := NewEngine(idx, reg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return NewForwardProxy(engine, reg, ca, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// serveDecrypted should reject an unknown agent with 403 when default_action=BLOCK,
// without ever contacting an upstream.
func TestForwardProxy_DenyUnknownAgent(t *testing.T) {
	idx := &config.Index{
		AgentPolicies: map[string]map[string][]*config.Policy{},
		DefaultAction: config.ActionBlock,
	}
	fp := testForwardProxy(t, idx)
	prov, _ := providers.NewRegistry().Get("openai")

	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set(AgentIDHeader, "ghost-agent")

	resp := runServeDecrypted(t, fp, req, "api.openai.com", prov)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// A request missing the agent header is a 400.
func TestForwardProxy_MissingAgentHeader(t *testing.T) {
	idx := &config.Index{AgentPolicies: map[string]map[string][]*config.Policy{}, DefaultAction: config.ActionAllow}
	fp := testForwardProxy(t, idx)
	prov, _ := providers.NewRegistry().Get("openai")

	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))

	resp := runServeDecrypted(t, fp, req, "api.openai.com", prov)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// The forward proxy only speaks CONNECT.
func TestForwardProxy_RejectsNonConnect(t *testing.T) {
	idx := &config.Index{AgentPolicies: map[string]map[string][]*config.Policy{}, DefaultAction: config.ActionBlock}
	fp := testForwardProxy(t, idx)

	rec := httptest.NewRecorder()
	fp.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// runServeDecrypted drives serveDecrypted over an in-memory pipe and returns the
// HTTP response written back to the client side.
func runServeDecrypted(t *testing.T, fp *ForwardProxy, req *http.Request, host string, prov providers.Provider) *http.Response {
	t.Helper()
	client, server := net.Pipe()
	go func() {
		fp.serveDecrypted(server, req, host, prov)
		server.Close()
	}()
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	client.Close()
	return resp
}
