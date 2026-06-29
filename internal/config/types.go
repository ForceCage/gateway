package config

import "time"

// Action defines what the gateway does when a limit is reached.
type Action string

const (
	ActionBlock    Action = "BLOCK"
	ActionThrottle Action = "THROTTLE"
)

// Policy is a single spending or rate limit rule.
type Policy struct {
	Provider string   `yaml:"provider"`
	Models   []string `yaml:"models,omitempty"`
	Action   Action   `yaml:"action"`
	Limit    float64  `yaml:"limit"`
	Window   string   `yaml:"window"`

	// parsed from Window on load
	WindowDuration time.Duration `yaml:"-"`
}

// AgentConfig holds all policies for a single agent.
type AgentConfig struct {
	ID       string   `yaml:"id"`
	Policies []Policy `yaml:"policies"`
}

// Config is the top-level policy.yaml structure.
type Config struct {
	Version       string        `yaml:"version"`
	DefaultAction Action        `yaml:"default_action"`
	Agents        []AgentConfig `yaml:"agents"`
}

// Index is the O(1) runtime lookup structure built from Config at boot.
// Outer key: agent ID. Inner key: provider name.
type Index struct {
	// AgentPolicies maps agentID → providerName → []Policy
	AgentPolicies map[string]map[string][]*Policy
	DefaultAction Action
}
