package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads the YAML file at path and returns a ready-to-use Index.
func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse policy yaml: %w", err)
	}

	if cfg.DefaultAction == "" {
		cfg.DefaultAction = ActionBlock
	}

	idx := &Index{
		AgentPolicies: make(map[string]map[string][]*Policy, len(cfg.Agents)),
		DefaultAction: cfg.DefaultAction,
	}

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		providerMap := make(map[string][]*Policy, len(agent.Policies))

		for j := range agent.Policies {
			p := &agent.Policies[j]
			dur, err := time.ParseDuration(p.Window)
			if err != nil {
				return nil, fmt.Errorf("agent %q policy %d: invalid window %q: %w", agent.ID, j, p.Window, err)
			}
			p.WindowDuration = dur
			providerMap[p.Provider] = append(providerMap[p.Provider], p)
		}

		idx.AgentPolicies[agent.ID] = providerMap
	}

	return idx, nil
}

// LookupPolicies returns the policies for (agentID, provider), plus whether any
// agent entry exists at all.
func (idx *Index) LookupPolicies(agentID, provider string) ([]*Policy, bool) {
	providerMap, agentFound := idx.AgentPolicies[agentID]
	if !agentFound {
		return nil, false
	}
	return providerMap[provider], true
}
