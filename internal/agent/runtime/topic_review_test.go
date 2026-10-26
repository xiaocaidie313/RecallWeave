package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
	"recallweave/internal/skill"
)

// This tests packaged-skill/tool wiring, not model decision classification.
func TestPackagedTopicReviewWorkflow(t *testing.T) {
	catalog, err := skill.Open(filepath.Join("..", "..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := catalog.Load("topic-review")
	if err != nil || strings.TrimSpace(body) == "" {
		t.Fatalf("packaged skill unavailable: %v", err)
	}
	for _, tc := range []struct {
		name, original string
		toolErr        error
	}{
		{"original", `{"session_id":3,"messages":[{"seq":1,"role":"user","content":"英语学习计划改为每天练口语"}]}`, nil},
		{"empty", `{"session_id":3,"messages":[]}`, nil},
		{"failure", "", errors.New("source unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := agent.NewAbility("memory", "检索", "", agent.ToolBox{
				{Schema: llm.ToolSchema{Name: "search_memories"}, Run: func(context.Context, string) (string, error) {
					return `{"memories":[{"memory_id":7,"session_id":3,"seq_start":1,"seq_end":1,"title":"英语学习"}]}`, nil
				}},
				{Schema: llm.ToolSchema{Name: "get_messages"}, Run: func(context.Context, string) (string, error) {
					return tc.original, tc.toolErr
				}},
			})
			model := &scriptedModel{turns: []llm.Turn{
				{ToolCalls: []llm.ToolCall{
					{ID: "skill", Name: "load_skill", Arguments: `{"name":"topic-review"}`},
					{ID: "search", Name: "search_memories", Arguments: `{"keyword":"英语学习"}`},
				}},
				{ToolCalls: []llm.ToolCall{{ID: "source", Name: "get_messages", Arguments: `{"session_id":3,"seq_start":1,"seq_end":1}`}}},
				{Content: "测试结束"},
			}}
			a := agent.NewAgent(1, "recall", model, "", 3, memory).WithSkills(catalog)
			if strings.Contains(a.Instructions(), body) {
				t.Fatal("full skill body must not be injected before loading")
			}
			result, err := Run(context.Background(), a, Request{Input: "整理我们讨论过的英语学习计划和变化"})
			if err != nil {
				t.Fatal(err)
			}
			steps := result.State.Steps
			if len(steps) != 3 {
				t.Fatalf("unexpected tool steps: %+v", steps)
			}
			var loaded struct {
				Instructions string `json:"instructions"`
			}
			if err := json.Unmarshal([]byte(steps[0].Result), &loaded); err != nil || loaded.Instructions != body || steps[0].Error != "" {
				t.Fatalf("actual packaged skill did not load: %+v, %v", steps[0], err)
			}
			if steps[2].ToolName != "get_messages" || (steps[2].Error != "") != (tc.toolErr != nil) {
				t.Fatalf("original retrieval outcome lost: %+v", steps[2])
			}
			if tc.toolErr == nil && steps[2].Result != tc.original {
				t.Fatalf("original retrieval changed: %+v", steps[2])
			}
		})
	}
}
