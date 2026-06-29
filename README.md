# ForceCage Gateway

ForceCage Gateway is an open-source, high-performance, inline financial proxy and policy firewall designed specifically for autonomous AI agent networks and distributed microservices.

By sitting inline at your network boundary, ForceCage intercepts outgoing requests to metered API providers (like OpenAI and Anthropic), evaluates usage metrics against local policies in real time, and enforces hard programmatic spending limits before execution loops generate massive billing shocks.

## Features

- **Redis-Backed Real-Time Enforcement:** Atomic, sub-millisecond budget tracking using a Lua check-and-reserve script — no race conditions, no double-spend.
- **Dual-Phase Cost Control:** Conservative pre-flight estimation blocks over-budget requests before they hit the upstream. Post-execution reconciliation adjusts the running total to the exact billed amount.
- **O(1) Policy Lookup:** Policies are parsed once at boot and indexed into nested hash maps — enforcement latency is flat regardless of how many agents or rules exist.
- **Stateless Gateway:** All state lives in Redis. Scale horizontally behind any load balancer with zero coordination overhead.
- **Static YAML Configuration:** Explicit, version-controlled definitions for agent budgets, spending windows, and model permissions.
- **Frictionless Local Quickstart:** A single `docker compose up` spins up the gateway and a Redis instance side-by-side.

## Architecture

```
[Agent App]
    │
    │  POST /proxy/openai/v1/chat/completions
    │  X-ForceCage-Agent-ID: my-agent
    ▼
[ForceCage Gateway]
    │
    ├─ O(1) policy lookup (in-memory map)
    ├─ Pre-flight cost estimation
    ├─ Atomic Redis check-and-reserve (Lua)  ──▶  [Redis]
    │
    │  (if budget OK)
    ▼
[OpenAI / Anthropic / ...]
    │
    ▼
[ForceCage Gateway]  ◀── parse actual usage from response
    │
    ├─ Reconcile estimate → actual in Redis
    ▼
[Agent App]
```

## Quickstart

```bash
git clone https://github.com/ForceCage/gateway.git
cd gateway
docker compose up -d
```

Point your agent's SDK at the gateway instead of the provider directly:

```bash
# OpenAI SDK
OPENAI_BASE_URL=http://localhost:8080/proxy/openai

# Anthropic SDK
ANTHROPIC_BASE_URL=http://localhost:8080/proxy/anthropic
```

Include the agent identity header on every request:

```
X-ForceCage-Agent-ID: my-agent-id
```

## Configuration

Edit `policy.yaml` before starting the gateway. The file is hot-reload-safe on restart.

```yaml
version: "1.0"

# BLOCK: unknown agents are denied. ALLOW: unknown agents pass through without enforcement.
default_action: BLOCK

agents:
  - id: "marketing-enrichment-agent"
    policies:
      - provider: "openai"
        models: ["gpt-4o", "o1-pro"]
        action: "BLOCK"
        limit: 50.00    # USD
        window: "1h"

      - provider: "anthropic"
        action: "BLOCK"
        limit: 25.00
        window: "24h"
```

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `POLICY_PATH` | `policy.yaml` | Path to the policy YAML file |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection URL (supports ElastiCache, Redis Cloud, etc.) |
| `LISTEN_ADDR` | `:8080` | Address and port the gateway listens on |

### Production Redis

Swap the local Redis container for any managed Redis instance by setting `REDIS_URL`:

```bash
REDIS_URL=redis://my-cluster.abc123.cache.amazonaws.com:6379 ./gateway
```

No image rebuild required. The gateway binary is completely stateless.

## API

All requests to the gateway follow the pattern:

```
POST /proxy/{provider}/{upstream_path}
X-ForceCage-Agent-ID: {agent-id}
```

The `{upstream_path}` is forwarded verbatim to the provider. For example:

```
/proxy/openai/v1/chat/completions  →  https://api.openai.com/v1/chat/completions
/proxy/anthropic/v1/messages       →  https://api.anthropic.com/v1/messages
```

### Budget Exceeded Response

When a request would exceed a configured limit, the gateway returns `HTTP 429`:

```json
{
  "error": {
    "type": "budget_exceeded",
    "message": "agent \"my-agent\" has exceeded its 1h budget for openai (50.0012 / 50.0000 USD)",
    "current_spend": 50.0012,
    "limit": 50.00,
    "window": "1h"
  }
}
```

## Supported Providers

| Provider key | Upstream |
|---|---|
| `openai` | `https://api.openai.com` |
| `anthropic` | `https://api.anthropic.com` |

Additional providers can be added by implementing the `Provider` interface in `/internal/providers/`. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Health Check

```bash
curl http://localhost:8080/healthz
# → ok
```

## Security

If you discover a security vulnerability within ForceCage Gateway, please do not open a public issue. Instead, email your disclosure report directly to security@forcecage.com.
