package providers

import (
	"fmt"
	"net/http"
)

// Provider encapsulates provider-specific cost estimation and upstream routing.
type Provider interface {
	// Name returns the canonical provider key (e.g. "openai", "anthropic").
	Name() string
	// UpstreamBase returns the base URL to forward requests to (no trailing slash).
	UpstreamBase() string
	// EstimateCost returns a conservative pre-flight cost estimate in USD.
	// body is the raw request body.
	EstimateCost(r *http.Request, body []byte) (usd float64, err error)
	// ExtractActualCost parses the response body and returns the true cost in USD.
	// Returns 0 if the response does not contain usage data.
	ExtractActualCost(resp *http.Response, body []byte) (usd float64, err error)
}

// Registry holds all registered providers, keyed by name.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a registry pre-populated with built-in providers.
func NewRegistry() *Registry {
	r := &Registry{providers: make(map[string]Provider)}
	r.Register(&OpenAI{})
	r.Register(&Anthropic{})
	return r
}

// Register adds or replaces a provider.
func (r *Registry) Register(p Provider) {
	r.providers[p.Name()] = p
}

// Get returns the provider for the given name, or an error if unknown.
func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return p, nil
}
