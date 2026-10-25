package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"recallweave/internal/llm"
	"recallweave/internal/skill"
)

// skillTools are per-run infrastructure, not an Ability. References can only
// be read after their Skill was loaded in this run.
func skillTools(catalog *skill.Catalog) []llm.Tool {
	loaded := make(map[string]bool)
	return []llm.Tool{skillLoadTool(catalog, loaded), skillReferenceTool(catalog, loaded)}
}

func skillLoadTool(catalog *skill.Catalog, loaded map[string]bool) llm.Tool {
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
			loaded[args.Name] = true
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

func skillReferenceTool(catalog *skill.Catalog, loaded map[string]bool) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        "read_skill_reference",
			Description: "按需读取已加载 Skill 的 references/ 文本文件。先调用 load_skill；仅当操作说明要求或任务确实需要时调用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "已加载的 Skill 名称"},
					"path": map[string]any{"type": "string", "description": "相对路径，例如 references/evidence-checklist.md"},
				},
				"required": []string{"name", "path"},
			},
		},
		Run: func(_ context.Context, arguments string) (string, error) {
			var args struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return "", fmt.Errorf("invalid reference arguments: %w", err)
			}
			if !loaded[args.Name] {
				return "", fmt.Errorf("skill %q must be loaded before reading references", args.Name)
			}
			content, err := catalog.ReadReference(args.Name, args.Path)
			if err != nil {
				return "", err
			}
			result, err := json.Marshal(struct {
				Name    string `json:"name"`
				Path    string `json:"path"`
				Content string `json:"content"`
			}{Name: args.Name, Path: args.Path, Content: content})
			if err != nil {
				return "", err
			}
			return string(result), nil
		},
	}
}
