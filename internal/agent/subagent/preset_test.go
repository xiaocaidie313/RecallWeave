package subagent

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
)

func validPreset() Preset {
	return Preset{
		Name: "auditor", Description: "核查给定资料", Instructions: "只核查原文，不扩展调查范围。",
		AllowedTools: []string{"get_messages"},
	}
}

func TestManagerValidatesPresetConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"negative default rounds", func(c *Config) { c.MaxRounds = -1 }},
		{"excessive default rounds", func(c *Config) { c.MaxRounds = 21 }},
		{"negative default timeout", func(c *Config) { c.Timeout = -time.Second }},
		{"excessive default timeout", func(c *Config) { c.Timeout = 5*time.Minute + time.Nanosecond }},
		{"empty presets", func(c *Config) { c.Presets = []Preset{} }},
		{"missing default", func(c *Config) { c.DefaultAgent = "missing" }},
		{"duplicate name", func(c *Config) { c.Presets = append(c.Presets, c.Presets[0]) }},
		{"empty name", func(c *Config) { c.Presets[0].Name = "" }},
		{"uppercase name", func(c *Config) { c.Presets[0].Name = "Auditor" }},
		{"invalid name", func(c *Config) { c.Presets[0].Name = "audit/path" }},
		{"long name", func(c *Config) { c.Presets[0].Name = strings.Repeat("a", 41) }},
		{"empty description", func(c *Config) { c.Presets[0].Description = " \n " }},
		{"long description", func(c *Config) { c.Presets[0].Description = strings.Repeat("a", 501) }},
		{"invalid UTF8 description", func(c *Config) { c.Presets[0].Description = string([]byte{0xff}) }},
		{"empty instructions", func(c *Config) { c.Presets[0].Instructions = "\t" }},
		{"long instructions", func(c *Config) { c.Presets[0].Instructions = strings.Repeat("a", (8<<10)+1) }},
		{"invalid UTF8 instructions", func(c *Config) { c.Presets[0].Instructions = string([]byte{0xff}) }},
		{"negative preset rounds", func(c *Config) { c.Presets[0].MaxRounds = -1 }},
		{"excessive preset rounds", func(c *Config) { c.Presets[0].MaxRounds = 21 }},
		{"negative preset timeout", func(c *Config) { c.Presets[0].Timeout = -time.Second }},
		{"excessive preset timeout", func(c *Config) { c.Presets[0].Timeout = 5*time.Minute + time.Nanosecond }},
		{"no tools", func(c *Config) { c.Presets[0].AllowedTools = nil }},
		{"duplicate tools", func(c *Config) { c.Presets[0].AllowedTools = []string{"get_messages", "get_messages"} }},
		{"invalid tool name", func(c *Config) { c.Presets[0].AllowedTools = []string{"get.messages"} }},
		{"empty tool name", func(c *Config) { c.Presets[0].AllowedTools = []string{""} }},
	}
	for _, blocked := range []string{"delegate_task", "update_plan", "load_skill", "read_skill_reference", "save_artifact", "deactivate_memory"} {
		tests = append(tests, struct {
			name string
			edit func(*Config)
		}{"blocked " + blocked, func(c *Config) { c.Presets[0].AllowedTools = []string{blocked} }})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{DefaultAgent: "auditor", Presets: []Preset{validPreset()}}
			test.edit(&config)
			if manager, err := NewManager(config); err == nil || manager != nil {
				t.Fatalf("invalid configuration accepted: manager=%+v err=%v", manager, err)
			}
		})
	}
}

func TestDefaultPresetsAndDefaultSelectionRemainCompatible(t *testing.T) {
	manager := testManager(t, Config{MaxRounds: 6, Timeout: 2 * time.Second})
	if manager.DefaultAgent() != "researcher" {
		t.Fatalf("unexpected default Agent: %q", manager.DefaultAgent())
	}
	metadata := manager.AvailableAgents()
	if len(metadata) != 2 || metadata[0].Name != "researcher" || metadata[1].Name != "reviewer" || metadata[0].Description == "" || metadata[1].Description == "" {
		t.Fatalf("missing role selection metadata: %+v", metadata)
	}
	for _, preset := range manager.presets {
		if preset.MaxRounds != 6 || preset.Timeout != 2*time.Second {
			t.Fatalf("unset role budget did not inherit manager defaults: %+v", preset)
		}
	}
	model := &managerTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "done"}, nil
	}}
	result, err := manager.Delegate(context.Background(), managerParent(model), runtime.DelegationRequest{Task: "legacy request without agent"})
	if err != nil || result.State.AgentName != "researcher" {
		t.Fatalf("legacy request did not select researcher: %+v err=%v", result, err)
	}
}

func TestPresetAndMetadataCannotMutateManagerConfiguration(t *testing.T) {
	preset := validPreset()
	config := Config{DefaultAgent: "auditor", Presets: []Preset{preset}}
	manager := testManager(t, config)
	config.Presets[0].Name = "changed"
	config.Presets[0].Instructions = "changed"
	config.Presets[0].AllowedTools[0] = "save_artifact"
	metadata := manager.AvailableAgents()
	metadata[0].Name = "changed"
	metadata[0].Description = "changed"
	stored := manager.presets["auditor"]
	if stored.Name != "auditor" || stored.Instructions != preset.Instructions || !slices.Equal(stored.AllowedTools, []string{"get_messages"}) {
		t.Fatalf("caller configuration mutated manager: %+v", stored)
	}
	again := manager.AvailableAgents()
	if again[0].Name != "auditor" || again[0].Description != preset.Description {
		t.Fatalf("caller metadata mutated manager: %+v", again)
	}
	firstDefaults := DefaultPresets()
	firstDefaults[0].AllowedTools[0] = "changed"
	if DefaultPresets()[0].AllowedTools[0] != "search_memories" {
		t.Fatal("DefaultPresets shared mutable tool lists")
	}
}

func TestCustomPresetUsesOnlyParentToolIntersection(t *testing.T) {
	toolCalls := map[string]int{}
	var exposed []string
	model := &managerTestModel{next: func(_ context.Context, _ *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
		if exposed != nil {
			return llm.Turn{Content: "checked"}, nil
		}
		for _, tool := range tools {
			exposed = append(exposed, tool.Name)
		}
		return llm.Turn{ToolCalls: []llm.ToolCall{
			{ID: "scoped", Name: "get_messages", Arguments: `{}`},
			{ID: "remote", Name: "mcp_custom_reader", Arguments: `{}`},
			{ID: "missing", Name: "mcp_not_in_parent", Arguments: `{}`},
			{ID: "extra", Name: "search_memories", Arguments: `{}`},
		}}, nil
	}}
	parent := managerParent(model,
		managerReadTool("get_messages", func(context.Context, string) (string, error) {
			toolCalls["get_messages"]++
			return "SCOPED_MESSAGES", nil
		}),
		managerReadTool("search_memories", func(context.Context, string) (string, error) {
			toolCalls["search_memories"]++
			return "GLOBAL_MEMORY", nil
		}),
		managerReadTool("mcp_custom_reader", func(context.Context, string) (string, error) {
			toolCalls["mcp_custom_reader"]++
			return "REMOTE_READ", nil
		}),
	)
	preset := validPreset()
	preset.AllowedTools = []string{"get_messages", "mcp_custom_reader", "mcp_not_in_parent"}
	preset.MaxRounds, preset.Timeout = 3, time.Second
	manager := testManager(t, Config{DefaultAgent: "auditor", Presets: []Preset{preset}})
	result, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Agent: "auditor", Task: "audit"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(exposed)
	if !reflect.DeepEqual(exposed, []string{"get_messages", "mcp_custom_reader", "update_plan"}) {
		t.Fatalf("role scope did not intersect parent tools: %v", exposed)
	}
	if result.State.AgentName != "auditor" || result.State.AgentID != 2 || len(result.State.Errors) != 2 || toolCalls["get_messages"] != 1 || toolCalls["mcp_custom_reader"] != 1 || toolCalls["search_memories"] != 0 {
		t.Fatalf("custom role escaped its tool scope: %+v calls=%v", result.State, toolCalls)
	}
	child := manager.newChild(parent.Agent, manager.presets["auditor"])
	if child.MaxRounds != 3 || !strings.Contains(child.Instructions(), preset.Instructions) || !strings.Contains(child.Instructions(), "外部工具与资料规则") {
		t.Fatal("custom role instructions, budget or external-tool classification lost")
	}
}

func TestReviewerHasIndependentRoleAndNarrowerToolScope(t *testing.T) {
	manager := testManager(t, Config{})
	var modelCalls int
	model := &managerTestModel{next: func(_ context.Context, _ *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
		modelCalls++
		var names []string
		for _, tool := range tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"get_messages", "mcp_tavily_extract", "update_plan"}) {
			t.Fatalf("reviewer gained researcher discovery tools: %v", names)
		}
		return llm.Turn{Content: "结论一有依据，结论二缺少原文支持。"}, nil
	}}
	var parentTools []llm.Tool
	for _, name := range DefaultPresets()[0].AllowedTools {
		parentTools = append(parentTools, managerReadTool(name, nil))
	}
	parent := managerParent(model, parentTools...)
	result, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Agent: "reviewer", Task: "核查给定结论", Context: "结论一和结论二，来源在会话8"})
	if err != nil || result.State.AgentName != "reviewer" || result.State.AgentID != 3 || modelCalls != 1 {
		t.Fatalf("reviewer did not execute independently: %+v err=%v", result.State, err)
	}
	child := manager.newChild(parent.Agent, manager.presets["reviewer"])
	if !strings.Contains(child.Instructions(), "只读核查员") || !strings.Contains(child.Instructions(), "未解决的缺口") {
		t.Fatal("reviewer did not receive its own task instructions")
	}
}

func TestUnknownPresetDoesNotStartChild(t *testing.T) {
	manager := testManager(t, Config{})
	model := &managerTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		t.Fatal("unknown preset invoked the model")
		return llm.Turn{}, nil
	}}
	result, err := manager.Delegate(context.Background(), managerParent(model), runtime.DelegationRequest{Agent: "unregistered", Task: "investigate"})
	if err == nil || result.State.RunID != "" || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown preset was admitted: %+v err=%v", result, err)
	}
}

func TestPresetBudgetOverridesDefaultBudget(t *testing.T) {
	preset := validPreset()
	preset.MaxRounds, preset.Timeout = 2, time.Second
	manager := testManager(t, Config{MaxRounds: 8, Timeout: time.Minute, DefaultAgent: "auditor", Presets: []Preset{preset}})
	resolved := manager.presets["auditor"]
	if resolved.MaxRounds != 2 || resolved.Timeout != time.Second {
		t.Fatalf("role budget was replaced by defaults: %+v", resolved)
	}
}
