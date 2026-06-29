package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/forcecage/gateway/internal/budget"
	"github.com/forcecage/gateway/internal/config"
	"github.com/forcecage/gateway/internal/providers"
)

const (
	// AgentIDHeader is the request header identifying the calling agent.
	AgentIDHeader = "X-ForceCage-Agent-ID"
	// pathPrefix is stripped from the request path before forwarding.
	pathPrefix = "/proxy"
)

type Handler struct {
	idx      *config.Index
	registry *providers.Registry
	tracker  *budget.Tracker
	logger   *slog.Logger
}

func New(idx *config.Index, registry *providers.Registry, tracker *budget.Tracker, logger *slog.Logger) *Handler {
	return &Handler{
		idx:      idx,
		registry: registry,
		tracker:  tracker,
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

	// Lookup policies — O(1).
	policies, agentKnown := h.idx.LookupPolicies(agentID, providerName)

	if !agentKnown {
		if h.idx.DefaultAction == config.ActionBlock {
			h.writeError(w, http.StatusForbidden, fmt.Sprintf("agent %q is not configured and default_action is BLOCK", agentID))
			return
		}
		// Default ALLOW: forward without enforcement.
		h.forward(provider, w, r, bodyBytes, agentID, nil)
		return
	}

	applicable := matchingPolicies(policies, modelFromBody(bodyBytes))
	if len(applicable) == 0 {
		// Agent is known but no policy applies to this provider/model — allow.
		h.forward(provider, w, r, bodyBytes, agentID, nil)
		return
	}

	// Pre-flight: estimate cost, check all applicable policies.
	estimatedCost, err := provider.EstimateCost(r, bodyBytes)
	if err != nil {
		h.logger.Warn("cost estimation failed, applying conservative estimate", "err", err)
		estimatedCost = 0.10
	}

	// Reserve against every applicable policy. If any policy blocks, roll back the
	// reservations already made for this request so they don't leak as phantom spend.
	reservations := make([]budget.Reservation, 0, len(applicable))
	for _, p := range applicable {
		allowed, res, currentSpend, err := h.tracker.CheckAndReserve(
			r.Context(), agentID, providerName, p.Limit, estimatedCost, p.WindowDuration,
		)
		if err != nil {
			h.logger.Error("redis check-and-reserve failed", "err", err)
			// Fail open on Redis errors to avoid blocking legitimate traffic.
			continue
		}
		if !allowed {
			h.releaseAll(r.Context(), reservations)
			h.logger.Info("budget exceeded, blocking request",
				"agent", agentID, "provider", providerName,
				"spend", currentSpend, "limit", p.Limit, "window", p.Window,
			)
			h.writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{
					"type":          "budget_exceeded",
					"message":       fmt.Sprintf("agent %q has exceeded its %s budget for %s (%.4f / %.4f USD)", agentID, p.Window, providerName, currentSpend, p.Limit),
					"current_spend": currentSpend,
					"limit":         p.Limit,
					"window":        p.Window,
				},
			})
			return
		}
		reservations = append(reservations, res)
	}

	h.forward(provider, w, r, bodyBytes, agentID, reservations)
}

func (h *Handler) releaseAll(ctx context.Context, reservations []budget.Reservation) {
	for _, res := range reservations {
		if err := h.tracker.Release(ctx, res); err != nil {
			h.logger.Error("release reservation failed", "err", err)
		}
	}
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
			if len(reservations) == 0 {
				return nil
			}
			return h.reconcile(resp, provider, agentID, reservations)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.logger.Error("upstream error", "err", err)
			h.writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		},
	}

	rp.ServeHTTP(w, r)
}

// reconcile reads the response body, extracts actual cost, and adjusts Redis.
func (h *Handler) reconcile(
	resp *http.Response,
	provider providers.Provider,
	agentID string,
	reservations []budget.Reservation,
) error {
	// Streaming responses (SSE) carry no parseable usage block here and must not be
	// buffered — doing so would defeat streaming. Leave the conservative estimate
	// reserved and pass the stream through untouched.
	if isStreaming(resp) {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	actualCost, err := provider.ExtractActualCost(resp, body)
	if err != nil || actualCost == 0 {
		// Can't determine actual cost — keep the estimate reserved.
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, res := range reservations {
		if reconcileErr := h.tracker.Reconcile(ctx, res, actualCost); reconcileErr != nil {
			h.logger.Error("reconcile failed", "err", reconcileErr)
		}
	}

	h.logger.Info("request completed",
		"agent", agentID,
		"provider", provider.Name(),
		"actual_usd", fmt.Sprintf("%.6f", actualCost),
	)
	return nil
}

func isStreaming(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// matchingPolicies returns the policies that apply to the given request model.
// A policy with no Models list applies to every model for its provider; a policy
// that lists Models applies only when the request model is in that list.
func matchingPolicies(policies []*config.Policy, model string) []*config.Policy {
	out := make([]*config.Policy, 0, len(policies))
	for _, p := range policies {
		if len(p.Models) == 0 {
			out = append(out, p)
			continue
		}
		for _, m := range p.Models {
			if m == model {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// modelFromBody extracts the "model" field from a JSON request body, if present.
func modelFromBody(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Model
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
