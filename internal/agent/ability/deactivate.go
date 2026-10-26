package ability

import (
	"context"
	"fmt"
	"strings"

	"recallweave/internal/llm"
)

func deactivateMemoryTool(manager memoryBackend) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        DeactivateMemoryToolName,
			Description: "仅当当前用户明确要求忘记、纠正或停止使用某条旧信息时停用对应记忆。先检索定位 ID；目标不明先澄清。不会删除原文。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "integer", "minimum": 1, "description": "检索结果中待停用的记忆 ID，不能猜测"},
					"reason":    map[string]any{"type": "string", "description": "当前用户明确要求停用的原因"},
				},
				"required": []string{"memory_id", "reason"},
			},
		},
		Run: func(ctx context.Context, arguments string) (string, error) {
			var args struct {
				MemoryID uint   `json:"memory_id"`
				Reason   string `json:"reason"`
			}
			if err := decodeArguments(arguments, &args); err != nil {
				return "", err
			}
			args.Reason = strings.TrimSpace(args.Reason)
			if args.MemoryID == 0 || args.Reason == "" {
				return "", fmt.Errorf("memory_id and reason are required")
			}
			item, err := manager.DeactivateMemory(ctx, args.MemoryID, args.Reason)
			if err != nil {
				return "", err
			}
			return ResultToString(map[string]any{
				"memory_id": item.ID, "status": item.Status,
				"reason": item.DeactivationReason, "deactivated_at": item.DeactivatedAt,
			})
		},
	}
}
