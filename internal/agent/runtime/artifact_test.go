package runtime

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/artifact"
	"recallweave/internal/llm"
)

// Scripted turns verify wiring, not the real model's authorization judgment.
func TestRunSaveArtifact(t *testing.T) {
	for _, save := range []bool{false, true} {
		name := "answer_only"
		if save {
			name = "save_requested"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := artifact.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			turns := []llm.Turn{{Content: "# 偏好\n喜欢马思唯，来源：会话 1 消息 23。"}}
			input := "整理偏好"
			if save {
				input = "将偏好整理保存为 Markdown 文件"
				turns = append([]llm.Turn{{ToolCalls: []llm.ToolCall{{
					ID: "save", Name: ability.SaveArtifactToolName,
					Arguments: `{"title":"偏好","markdown":"# 偏好\n喜欢马思唯，来源：会话 1 消息 23。"}`,
				}}}}, turns...)
			}
			a := agent.NewAgent(1, "test", &scriptedModel{turns: turns}, "", 3, ability.NewArtifactAbility(store))
			result, err := Run(context.Background(), a, Request{Input: input})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !save {
				if len(entries) != 0 || len(result.State.Steps) != 0 {
					t.Fatal("answer-only run must not create an artifact")
				}
				return
			}
			if len(entries) != 1 || len(result.State.Steps) != 1 {
				t.Fatalf("save tool not recorded: %+v", result.State.Steps)
			}
			step := result.State.Steps[0]
			var saved artifact.Saved
			if step.Error != "" || json.Unmarshal([]byte(step.Result), &saved) != nil || saved.ID == "" {
				t.Fatalf("invalid saved artifact in Run: %+v", step)
			}
			data, err := os.ReadFile(saved.Path)
			if err != nil || string(data) != turns[1].Content {
				t.Fatalf("Run output does not match saved file: %q, %v", data, err)
			}
		})
	}
}
