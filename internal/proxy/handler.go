package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/forcecage/gateway/internal/budget"
	"github.com/forcecage/gateway/internal/config"
	"github.com/forcecage/gateway/internal/providers"
)

// pathPrefix is stripped from the request path before forwarding.
const pathPrefix = "/proxy"

// Handler is the reverse-proxy ingress: agents point their SDK base_url at
// /proxy/{provider} and ForceCage enforces, forwards, and reconciles. All
// enforcement decisions are delegated to the shared Engine.
type Handler struct {
	engine   *Engine
	registry *providers.Registry
	logger   *slog.Logger
}

func New(idx *config.Index, registry *providers.Registry, tracker *budget.Tracker, logger *slog.Logger) *Handler {
	return &Handler{
		engine:   NewEngine(idx, registry, tracker, logger),
		registry: registry,
		logger:   logger,
	}
}

// ServeHTTP handles all proxy requests routed to /proxy/{provider}/...
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract provider from path: /proxy/{provider}/rest/of/path
	trimmed := strings.TrimPrefix(r.URL.Path, pathPrefix+"/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		h.writeError(w, http.StatusBadRequest, "missing provider in path (expected /proxy/{provider}/...)")
		return
	}
	providerName := parts[0]

	provider, err := h.registry.Get(providerName)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	agentID := r.Header.Get(AgentIDHeader)
	if agentID == "" {
		h.writeError(w, http.StatusBadRequest, AgentIDHeader+" header is required")
		return
	}

	// Buffer request body so we can (a) estimate cost and (b) still forward it.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	decision := h.engine.Authorize(r.Context(), agentID, providerName, provider, bodyBytes)
	if decision.Deny {
		h.writeJSON(w, decision.StatusCode, decision.Detail)
		return
	}

	h.forward(provider, w, r, bodyBytes, agentID, decision.Reservations)
}

// forward proxies the request to the upstream provider and reconciles cost afterwards.
func (h *Handler) forward(
	provider providers.Provider,
	w http.ResponseWriter,
	r *http.Request,
	bodyBytes []byte,
	agentID string,
	reservations []budget.Reservation,
) {
	upstreamBase, err := url.Parse(provider.UpstreamBase())
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "invalid upstream URL")
		return
	}

	// Strip /proxy/{provider} prefix, keep the rest.
	upstreamPath := strings.TrimPrefix(r.URL.Path, pathPrefix+"/"+provider.Name())

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamBase.Scheme
			req.URL.Host = upstreamBase.Host
			req.URL.Path = upstreamPath
			req.Host = upstreamBase.Host
			// Restore body for the outgoing request.
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
			// Remove gateway-internal headers.
			req.Header.Del(AgentIDHeader)
		},
		ModifyResponse: func(resp *http.Response) error {
			return h.engine.Reconcile(resp, provider, agentID, reservations)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.logger.Error("upstream error", "err", err)
			h.writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		},
	}

	rp.ServeHTTP(w, r)
}

func (h *Handler) writeError(w http.ResponseWriter, code int, msg string) {
	h.writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg}})
}

func (h *Handler) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
