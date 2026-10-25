package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"recallweave/internal/llm"
	"recallweave/internal/skill"
)

// skillLoadTool is agent infrastructure, not an Ability. It exposes one
// selected Skill's instructions without putting every body in the prompt.
func skillLoadTool(catalog *skill.Catalog) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        "load_skill",
			Description: "按名称加载一个本地 Skill 的完整操作说明。仅在任务与其描述匹配时调用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "要加载的 Skill 名称"},
				},
				"required": []string{"name"},
			},
		},
		Run: func(_ context.Context, arguments string) (string, error) {
			var args struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return "", fmt.Errorf("invalid skill arguments: %w", err)
			}
			body, err := catalog.Load(args.Name)
			if err != nil {
				return "", err
			}
			result, err := json.Marshal(struct {
				Name         string `json:"name"`
				Instructions string `json:"instructions"`
			}{Name: args.Name, Instructions: body})
			if err != nil {
				return "", err
			}
			return string(result), nil
		},
	}
}
