package main

import (
	"testing"

	"recallweave/internal/config"
)

func TestServerUsesBuiltinSubagentPresetsWhenOmitted(t *testing.T) {
	manager, err := newSubagentManager(config.SubagentsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	items := manager.AvailableAgents()
	if manager.DefaultAgent() != "researcher" || len(items) != 2 || items[0].Name != "researcher" || items[1].Name != "reviewer" {
		t.Fatalf("unexpected default presets: %+v default=%q", items, manager.DefaultAgent())
	}
}

func TestServerSubagentConfigurationReachesRegistry(t *testing.T) {
	manager, err := newSubagentManager(config.SubagentsConfig{
		DefaultAgent: "evidence_checker",
		Presets:      []config.SubagentPresetConfig{{Name: "evidence_checker", Description: "Check evidence", Instructions: "Check the supplied messages", Tools: []string{"get_messages"}, MaxRounds: 3, TimeoutSeconds: 15}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := manager.AvailableAgents()
	if manager.DefaultAgent() != "evidence_checker" || len(items) != 1 || items[0].Name != "evidence_checker" || items[0].Description != "Check evidence" {
		t.Fatalf("server ignored configured presets: %+v default=%q", items, manager.DefaultAgent())
	}
}

func TestServerRejectsInvalidSubagentConfigurationBeforeAssembly(t *testing.T) {
	for _, cfg := range []config.SubagentsConfig{
		{Presets: []config.SubagentPresetConfig{}},
		{DefaultAgent: "missing"},
		{DefaultAgent: "custom", Presets: []config.SubagentPresetConfig{{Name: "custom", Description: "desc", Instructions: "instructions", Tools: []string{"get_messages"}, TimeoutSeconds: 301}}},
	} {
		if _, err := newSubagentManager(cfg); err == nil {
			t.Fatalf("invalid registry accepted: %+v", cfg)
		}
	}
}
