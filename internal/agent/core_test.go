package agent

import (
	"strings"
	"testing"
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
