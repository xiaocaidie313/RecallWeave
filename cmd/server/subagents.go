package main

import (
	"time"

	"recallweave/internal/agent/subagent"
	"recallweave/internal/config"
)

// Validate PresetAgents before opening the database or connecting to MCP. YAML
// transport units stay in config; the manager receives domain-owned durations.
func newSubagentManager(cfg config.SubagentsConfig) (*subagent.Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	options := subagent.Config{DefaultAgent: cfg.DefaultAgent}
	if cfg.PresetAgents != nil {
		options.PresetAgents = make([]subagent.PresetAgent, len(cfg.PresetAgents))
		for i, PresetAgent := range cfg.PresetAgents {
			options.PresetAgents[i] = subagent.PresetAgent{
				Name: PresetAgent.Name, Description: PresetAgent.Description, Instructions: PresetAgent.Instructions,
				AllowedTools: PresetAgent.Tools, MaxRounds: PresetAgent.MaxRounds,
				Timeout: time.Duration(PresetAgent.TimeoutSeconds) * time.Second,
			}
		}
	}
	return subagent.NewManager(options)
}
