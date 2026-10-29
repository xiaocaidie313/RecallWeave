package main

import (
	"time"

	"recallweave/internal/agent/subagent"
	"recallweave/internal/config"
)

// Validate presets before opening the database or connecting to MCP. YAML
// transport units stay in config; the manager receives domain-owned durations.
func newSubagentManager(cfg config.SubagentsConfig) (*subagent.Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	options := subagent.Config{DefaultAgent: cfg.DefaultAgent}
	if cfg.Presets != nil {
		options.Presets = make([]subagent.Preset, len(cfg.Presets))
		for i, preset := range cfg.Presets {
			options.Presets[i] = subagent.Preset{
				Name: preset.Name, Description: preset.Description, Instructions: preset.Instructions,
				AllowedTools: preset.Tools, MaxRounds: preset.MaxRounds,
				Timeout: time.Duration(preset.TimeoutSeconds) * time.Second,
			}
		}
	}
	return subagent.NewManager(options)
}
