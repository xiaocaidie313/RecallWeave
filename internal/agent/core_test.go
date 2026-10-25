package agent

import (
	"context"
	"strings"
	"testing"

	"recallweave/internal/llm"
)

func TestInstructionsIncludeRoleAndAbilities(t *testing.T) {
	memory := NewAbility("memory", "检索记忆", "先用 search_memories", nil)
	a := NewAgent(1, "recall", nil, "记忆助手", 0, memory)

	got := a.Instructions()
	for _, want := range []string{"记忆助手", "memory: 检索记忆", "先用 search_memories"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q:\n%s", want, got)
		}
	}
	if a.Rounds() != defaultMaxRounds {
		t.Fatalf("expected default rounds %d, got %d", defaultMaxRounds, a.Rounds())
	}
}

func TestExternalToolsAreSeparateAndCannotShadowLocalTools(t *testing.T) {
	local := llm.Tool{Schema: llm.ToolSchema{Name: "search_memories"}, Run: func(context.Context, string) (string, error) { return "local", nil }}
	remote := llm.Tool{Schema: llm.ToolSchema{Name: "mcp_ping"}, Run: func(context.Context, string) (string, error) { return "remote", nil }}
	a := NewAgent(1, "recall", nil, "", 3, NewAbility("memory", "", "", ToolBox{local}))
	if err := a.WithExternalTools([]llm.Tool{remote}); err != nil {
		t.Fatal(err)
	}
	if len(a.Abilities) != 1 || len(a.ExternalTools) != 1 {
		t.Fatalf("external MCP tools must not become abilities: %+v", a)
	}
	result, err := a.Tools().Run(context.Background(), "mcp_ping", `{}`)
	if err != nil || result != "remote" {
		t.Fatalf("external tool unavailable: %q, %v", result, err)
	}
	if err := a.WithExternalTools([]llm.Tool{local}); err == nil {
		t.Fatal("external tool must not shadow an existing tool")
	}
}
