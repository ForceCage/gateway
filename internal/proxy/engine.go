package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/forcecage/gateway/internal/budget"
	"github.com/forcecage/gateway/internal/config"
	"github.com/forcecage/gateway/internal/providers"
)

// AgentIDHeader is the request header identifying the calling agent. It is read
// identically by every ingress adapter (reverse proxy, forward proxy, ...).
const AgentIDHeader = "X-ForceCage-Agent-ID"

// Engine is the transport-agnostic enforcement core: policy lookup, cost
// estimation, atomic reserve-with-rollback, and post-response reconciliation.
// Ingress adapters (handler.go, forward.go) resolve the provider and agent from
// their own wire format, then delegate the decision to the Engine so enforcement
// logic lives in exactly one place.
type Engine struct {
	idx      *config.Index
	registry *providers.Registry
	tracker  *budget.Tracker
	logger   *slog.Logger
}

func NewEngine(idx *config.Index, registry *providers.Registry, tracker *budget.Tracker, logger *slog.Logger) *Engine {
	return &Engine{idx: idx, registry: registry, tracker: tracker, logger: logger}
}

// Decision is the transport-agnostic result of an authorization check. When Deny
// is true the ingress renders StatusCode + Detail in its own wire format; when
// Deny is false the request may proceed and Reservations must be passed to
// Reconcile once the upstream response is available.
type Decision struct {
	Deny         bool
	StatusCode   int
	Detail       map[string]any
	Reservations []budget.Reservation
}

func denyDecision(status int, detail map[string]any) Decision {
	return Decision{Deny: true, StatusCode: status, Detail: detail}
}

// Authorize runs the full pre-flight check for one request against one provider.
func (e *Engine) Authorize(ctx context.Context, agentID, providerName string, provider providers.Provider, body []byte) Decision {
	policies, agentKnown := e.idx.LookupPolicies(agentID, providerName)

	if !agentKnown {
		if e.idx.DefaultAction == config.ActionBlock {
			return denyDecision(http.StatusForbidden, map[string]any{
				"error": map[string]any{
					"type":    "agent_not_configured",
					"message": fmt.Sprintf("agent %q is not configured and default_action is BLOCK", agentID),
				},
			})
		}
		return Decision{} // default ALLOW: forward unenforced
	}

	applicable := matchingPolicies(policies, modelFromBody(body))
	if len(applicable) == 0 {
		return Decision{} // known agent, no applicable policy: forward unenforced
	}

	estimatedCost, err := provider.EstimateCost(&http.Request{}, body)
	if err != nil {
		e.logger.Warn("cost estimation failed, applying conservative estimate", "err", err)
		estimatedCost = 0.10
	}

	// Reserve against every applicable policy. If any policy blocks, roll back the
	// reservations already made so they don't leak as phantom spend.
	reservations := make([]budget.Reservation, 0, len(applicable))
	for _, p := range applicable {
		allowed, res, currentSpend, err := e.tracker.CheckAndReserve(
			ctx, agentID, providerName, p.Limit, estimatedCost, p.WindowDuration,
		)
		if err != nil {
			e.logger.Error("redis check-and-reserve failed", "err", err)
			// Fail open on Redis errors to avoid blocking legitimate traffic.
			continue
		}
		if !allowed {
			e.releaseAll(ctx, reservations)
			e.logger.Info("budget exceeded, blocking request",
				"agent", agentID, "provider", providerName,
				"spend", currentSpend, "limit", p.Limit, "window", p.Window,
			)
			return denyDecision(http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{
					"type":          "budget_exceeded",
					"message":       fmt.Sprintf("agent %q has exceeded its %s budget for %s (%.4f / %.4f USD)", agentID, p.Window, providerName, currentSpend, p.Limit),
					"current_spend": currentSpend,
					"limit":         p.Limit,
					"window":        p.Window,
				},
			})
		}
		reservations = append(reservations, res)
	}

	return Decision{Reservations: reservations}
}

func (e *Engine) releaseAll(ctx context.Context, reservations []budget.Reservation) {
	for _, res := range reservations {
		if err := e.tracker.Release(ctx, res); err != nil {
			e.logger.Error("release reservation failed", "err", err)
		}
	}
}

// Reconcile reads the upstream response, extracts the actual cost, and adjusts
// each reservation from the conservative estimate to the true amount.
func (e *Engine) Reconcile(resp *http.Response, provider providers.Provider, agentID string, reservations []budget.Reservation) error {
	if len(reservations) == 0 {
		return nil
	}
	// Streaming responses (SSE) carry no parseable usage block here and must not be
	// buffered — doing so would defeat streaming. Leave the estimate reserved.
	if isStreaming(resp) {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	resp.Body = io.NopCloser(strings.NewReader(string(body)))

	actualCost, err := provider.ExtractActualCost(resp, body)
	if err != nil || actualCost == 0 {
		return nil // can't determine actual cost — keep the estimate reserved
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, res := range reservations {
		if reconcileErr := e.tracker.Reconcile(ctx, res, actualCost); reconcileErr != nil {
			e.logger.Error("reconcile failed", "err", reconcileErr)
		}
	}

	e.logger.Info("request completed",
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
