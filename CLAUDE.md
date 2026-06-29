# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build ./...

# Run the gateway (requires Redis at REDIS_URL)
go run ./cmd/gateway

# Vet
go vet ./...

# Run all tests
go test ./...

# Run a single package's tests
go test ./internal/budget/...

# Run a single test by name
go test ./internal/proxy/... -run TestHandlerBudgetExceeded

# Docker local stack (gateway + Redis)
docker compose up -d
docker compose logs -f gateway
```

Environment variables for local runs (all optional):

| Variable | Default |
|---|---|
| `POLICY_PATH` | `policy.yaml` |
| `REDIS_URL` | `redis://localhost:6379` |
| `LISTEN_ADDR` | `:8080` |

## Architecture

ForceCage is a **single enforcement engine behind pluggable ingress adapters**. The engine — parse the request, estimate cost, atomically check-and-reserve against Redis, enforce, forward, reconcile — is independent of *how* traffic reaches it. Today one ingress is implemented (reverse proxy); the others are roadmap, but the core (`internal/proxy`, `internal/budget`, `internal/providers`, `internal/config`) is deliberately ingress-agnostic so adding an adapter does not touch enforcement logic.

Enforcement must live **out-of-process at a chokepoint the workload cannot bypass** — that is what makes ForceCage a firewall rather than an observability library. An in-process SDK shim is convenient but trivially bypassed (a prompt-injection or a stray HTTP client sidesteps it) and cannot share budget state across horizontally-scaled replicas, so it is only ever a dev-convenience layer, never the enforcement boundary.

### Ingress modes (pluggable)

| Mode | How the agent points at it | Dev friction | Bypass-proof | Status |
|---|---|---|---|---|
| **Reverse proxy** | SDK `base_url` → `/proxy/{provider}` | One env var | Yes | **Implemented** |
| **Forward proxy** | `HTTPS_PROXY` + trusted CA, MITM the CONNECT tunnel | One env var, no code | Yes | Roadmap |
| **Sidecar / egress gateway** | Envoy `ext_authz`, service mesh, or NAT-forced egress | Zero (infra-enforced) | Yes, infra-level | Roadmap |
| **In-process SDK shim** | `import` a package | Code change | **No** | Convenience only, not security |

All modes feed the same lifecycle below. Only the first three steps (how the request is received and which provider/agent it maps to) differ per adapter; steps 4–7 are shared engine code. AWS Bedrock is a forward-proxy/SigV4 case: the agent does not own the egress and requests are SigV4-signed, so it is enforced via VPC egress chokepoint (PrivateLink) or a re-signing proxy reading `x-amzn-bedrock-*` usage headers — not via `base_url`.

### Request lifecycle

```
POST /proxy/{provider}/...
X-ForceCage-Agent-ID: {agent-id}
        │
        ▼
internal/proxy/handler.go
  1. Extract provider name from path, agent ID from header
  2. O(1) policy lookup: idx.AgentPolicies[agentID][provider]
  3. Buffer request body
  4. provider.EstimateCost() → conservative pre-flight USD estimate
  5. budget.CheckAndReserve() → atomic Lua script in Redis (check + INCRBYFLOAT)
     └─ over limit → HTTP 429, stop
  6. httputil.ReverseProxy forwards to upstream (strips /proxy/{provider} prefix)
  7. ModifyResponse: provider.ExtractActualCost() → budget.Reconcile() adjusts delta
```

### Key packages

**`internal/config`** — Policy loading and O(1) indexing. `loader.go` parses `policy.yaml` once at boot and builds `Index.AgentPolicies`: a `map[agentID]map[providerName][]*Policy`. All time windows are pre-parsed to `time.Duration`. No file watching — restart to reload.

**`internal/budget`** — Redis state. `tracker.go` uses a fixed-window key `fc:budget:{agentID}:{provider}:{bucketInt}` where `bucketInt = unixNano / windowNanos`. A single embedded Lua script (`checkAndReserve`) does the atomic check-then-increment to prevent race conditions across horizontally scaled instances. `Reconcile()` adjusts the bucket with `INCRBYFLOAT(delta)` after the actual response is parsed.

**`internal/providers`** — Provider interface and registry. Each provider implements:
- `EstimateCost(r, body)` — parse request JSON, count input chars (÷3 for conservative token estimate), add `max_tokens` for output, multiply by pricing table.
- `ExtractActualCost(resp, body)` — parse response JSON for actual `usage` counts, compute exact USD.

To add a new provider: implement the `Provider` interface, add a pricing map, register in `NewRegistry()` in `provider.go`.

**`internal/proxy`** — Single file (`handler.go`). The `ReverseProxy.Director` strips the `/proxy/{provider}` path prefix and rewrites the host. `ModifyResponse` runs the reconciliation. Redis errors fail open (request is forwarded) to avoid blocking legitimate traffic on transient Redis outages.

### Failure modes by design

- **Redis down**: `CheckAndReserve` errors are logged and skipped — the request is forwarded. This is intentional to avoid Redis being a hard availability dependency.
- **Unknown agent + `default_action: BLOCK`**: returns HTTP 403.
- **Unknown agent + `default_action: ALLOW`**: forwarded without enforcement.
- **Actual cost > estimated cost**: reconciled silently after the fact; the overage counts against the next window check.
