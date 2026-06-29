package providers

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

// GetByHost returns the provider whose upstream host matches the given host
// (e.g. "api.openai.com"). Used by the forward-proxy ingress, which identifies
// the provider from the CONNECT target rather than a path prefix.
func (r *Registry) GetByHost(host string) (Provider, error) {
	// Strip any port suffix (host:443).
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, p := range r.providers {
		u, err := url.Parse(p.UpstreamBase())
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Host, host) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no provider registered for host %q", host)
}
