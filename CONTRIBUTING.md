# Contributing to ForceCage

We love community involvement. To protect the stability and performance of security-critical infrastructure, all changes go through a structured validation workflow.

## Contribution Workflow

We do not allow direct write access to main branches. All changes must go through a community-reviewed pull request.

1. **Fork the repository.** Create a copy of `ForceCage/gateway` under your own GitHub account.

2. **Implement your provider.** If you are adding support for a new metered API, create a new file under `internal/providers/`. Your struct must implement the `Provider` interface:

   ```go
   type Provider interface {
       Name() string
       UpstreamBase() string
       EstimateCost(r *http.Request, body []byte) (float64, error)
       ExtractActualCost(resp *http.Response, body []byte) (float64, error)
   }
   ```

   Register your provider in `NewRegistry()` inside `internal/providers/provider.go`.

3. **Write tests.** New providers must include unit tests covering at least:
   - Cost estimation for a representative request payload.
   - Actual cost extraction from a representative response payload.
   - Edge cases: empty body, unknown model, missing usage field.

4. **Sign your commits.** We require a Developer Certificate of Origin (DCO). Sign off on every commit:

   ```bash
   git commit -s -m "providers: add ZoomInfo contact lookup provider"
   ```

5. **Open a pull request** against the `main` branch with a clear description of what the provider covers and how pricing was sourced.

## Code Review Gates

Every PR goes through:

- **Automated linting and build checks** via GitHub Actions. Your PR must compile cleanly with `go build ./...` and pass `go vet ./...`.
- **Human architectural sign-off** from a core maintainer, verifying that the change maintains sub-millisecond proxy overhead and introduces no supply-chain vulnerabilities.

## Adding a Provider: Example Skeleton

```go
package providers

import (
    "encoding/json"
    "net/http"
)

var myProviderPricing = map[string][2]float64{
    "my-model-v1": {1.00, 5.00}, // input / output per 1M tokens
    "_default":    {5.00, 20.00},
}

type MyProvider struct{}

func (p *MyProvider) Name() string         { return "myprovider" }
func (p *MyProvider) UpstreamBase() string { return "https://api.myprovider.com" }

func (p *MyProvider) EstimateCost(r *http.Request, body []byte) (float64, error) {
    // Parse body, estimate conservatively (overestimate input tokens, assume max_tokens for output).
    return 0.01, nil
}

func (p *MyProvider) ExtractActualCost(resp *http.Response, body []byte) (float64, error) {
    // Parse response body for actual usage counts.
    return 0, nil
}
```
