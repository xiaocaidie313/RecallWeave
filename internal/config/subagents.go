package config

import "fmt"

// SubagentsConfig contains configuration data only. A nil Presets slice selects
// built-in defaults; an explicitly empty slice is left for domain validation.
type SubagentsConfig struct {
	DefaultAgent string                 `yaml:"default_agent"`
	Presets      []SubagentPresetConfig `yaml:"presets"`
}

type SubagentPresetConfig struct {
	Name           string   `yaml:"name"`
	Description    string   `yaml:"description"`
	Instructions   string   `yaml:"instructions"`
	Tools          []string `yaml:"tools"`
	MaxRounds      int      `yaml:"max_rounds"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

// Validate bounds numeric values before seconds are converted to time.Duration.
// Zero means inherit the manager defaults; names and tool policy belong to the
// subagent package. Errors identify fields without echoing configuration data.
func (c SubagentsConfig) Validate() error {
	for i, preset := range c.Presets {
		if preset.MaxRounds < 0 || preset.MaxRounds > 20 {
			return fmt.Errorf("subagents.presets[%d].max_rounds must be between 0 and 20", i)
		}
		if preset.TimeoutSeconds < 0 || preset.TimeoutSeconds > 300 {
			return fmt.Errorf("subagents.presets[%d].timeout_seconds must be between 0 and 300", i)
		}
	}
	return nil
}
