package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadSubagentTestConfig(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadSubagentPresetAgents(t *testing.T) {
	cfg, err := loadSubagentTestConfig(t, `server:
  port: 8008
subagents:
  default_agent: reviewer
  PresetAgents:
    - name: reviewer
      description: Check the supplied evidence.
      instructions: Report conclusions and gaps.
      tools:
        - get_messages
        - mcp_tavily_extract
      max_rounds: 3
      timeout_seconds: 20
`)
	if err != nil {
		t.Fatal(err)
	}
	want := SubagentsConfig{
		DefaultAgent: "reviewer",
		PresetAgents: []SubagentPresetAgentConfig{{
			Name: "reviewer", Description: "Check the supplied evidence.",
			Instructions: "Report conclusions and gaps.",
			Tools:        []string{"get_messages", "mcp_tavily_extract"},
			MaxRounds:    3, TimeoutSeconds: 20,
		}},
	}
	if !reflect.DeepEqual(cfg.Subagents, want) {
		t.Fatalf("unexpected subagent configuration: %+v", cfg.Subagents)
	}
	if cfg.Server.Port != 8008 {
		t.Fatal("loading subagent configuration changed unrelated server settings")
	}
}

func TestLoadSubagentPresetAgentsPreservesDefaultSelection(t *testing.T) {
	for _, content := range []string{"server:\n  port: 8008\n", "subagents: {}\n", "subagents:\n  PresetAgents: null\n"} {
		cfg, err := loadSubagentTestConfig(t, content)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Subagents.PresetAgents != nil || cfg.Subagents.DefaultAgent != "" {
			t.Fatal("omitted PresetAgents must remain nil for manager defaults")
		}
	}
	cfg, err := loadSubagentTestConfig(t, "subagents:\n  PresetAgents: []\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Subagents.PresetAgents == nil || len(cfg.Subagents.PresetAgents) != 0 {
		t.Fatal("an explicit empty PresetAgent list must remain distinguishable from omission")
	}
}

func TestLoadSubagentPresetAgentsRejectsInvalidBudgetsWithoutEchoingConfig(t *testing.T) {
	for _, test := range []struct {
		name, budget, field string
	}{
		{"negative rounds", "max_rounds: -1", "max_rounds"},
		{"excess rounds", "max_rounds: 21", "max_rounds"},
		{"negative timeout", "timeout_seconds: -1", "timeout_seconds"},
		{"excess timeout", "timeout_seconds: 301", "timeout_seconds"},
		{"duration overflow", "timeout_seconds: 9223372036854775807", "timeout_seconds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := loadSubagentTestConfig(t, "subagents:\n  PresetAgents:\n    - name: private-marker\n      instructions: private-test-content\n      "+test.budget+"\n")
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("expected a field-specific budget error, got %v", err)
			}
			if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "private-test-content") {
				t.Fatal("budget error echoed configuration data")
			}
			if cfg.Subagents.PresetAgents != nil {
				t.Fatal("invalid configuration must not return populated PresetAgents")
			}
		})
	}
}

func TestLoadSubagentPresetAgentsAcceptsInheritedAndMaximumBudgets(t *testing.T) {
	for _, budget := range []string{
		"max_rounds: 0\n      timeout_seconds: 0",
		"max_rounds: 20\n      timeout_seconds: 300",
	} {
		if _, err := loadSubagentTestConfig(t, "subagents:\n  PresetAgents:\n    - name: test\n      "+budget+"\n"); err != nil {
			t.Fatalf("valid budget rejected: %v", err)
		}
	}
}
