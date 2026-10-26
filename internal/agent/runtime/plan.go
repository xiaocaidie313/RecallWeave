package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"recallweave/internal/llm"
	"recallweave/internal/plan"
)

const planningInstructions = `需要多步执行、比较或处理不确定结果的任务，使用 update_plan 建立简短计划，并将当前步骤标为 in_progress，再调用业务工具。简单问题直接回答。
update_plan 每次提交完整计划。根据工具的实际结果更新步骤；遇到失败可重试、修改后续步骤，或标记 blocked 并说明原因。调整目标或步骤安排时填写 explanation。可将计划更新与业务工具调用放在同一轮，按先更新、后执行的顺序排列。
结束前将步骤更新为 completed、blocked 或 skipped，填写结果或原因；blocked/skipped 的部分在回答中说明。计划是进度记录，写计划或标记完成本身不代表工具已执行。`

// planTool 每次 Run 单独创建，计划仅属于当前运行，不写入共享 Agent。
func planTool(state *RunState) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        plan.ToolName,
			Description: "创建或更新当前任务的完整计划。多步任务按结果更新进度，必要时调整后续步骤。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal":        map[string]any{"type": "string", "description": "当前任务目标"},
					"explanation": map[string]any{"type": "string", "description": "修改目标或步骤安排时必填，说明调整原因"},
					"steps": map[string]any{
						"type": "array", "minItems": 1, "maxItems": 10,
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"step":   map[string]any{"type": "string", "description": "步骤描述"},
								"status": map[string]any{"type": "string", "enum": []string{plan.Pending, plan.InProgress, plan.Completed, plan.Blocked, plan.Skipped}},
								"result": map[string]any{"type": "string", "description": "完成结果或无法完成的原因；completed/blocked/skipped 必填"},
							},
							"required": []string{"step", "status"},
						},
					},
				},
				"required": []string{"goal", "steps"},
			},
		},
		Run: func(_ context.Context, arguments string) (string, error) {
			var args struct {
				Goal        string `json:"goal"`
				Explanation string `json:"explanation"`
				Steps       []struct {
					Step   string `json:"step"`
					Status string `json:"status"`
					Result string `json:"result"`
				} `json:"steps"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return "", fmt.Errorf("plan: invalid arguments: %w", err)
			}
			// 创建解析 下一个plan 
			next := plan.Plan{Goal: strings.TrimSpace(args.Goal), Steps: make([]plan.Step, len(args.Steps))}
			for i, step := range args.Steps {
				next.Steps[i] = plan.Step{Step: strings.TrimSpace(step.Step), Status: step.Status, Result: strings.TrimSpace(step.Result)}
			}
			if err := next.Validate(); err != nil {
				return "", err
			}
			if state.Plan != nil && arrangementChanged(*state.Plan, next) && strings.TrimSpace(args.Explanation) == "" {
				return "", fmt.Errorf("plan: changing the goal or step arrangement requires explanation")
			}
			encoded, err := json.Marshal(next)
			if err != nil {
				return "", err
			}
			state.Plan = &next
			return string(encoded), nil
		},
	}
}

// plan 发生改变
func arrangementChanged(previous, next plan.Plan) bool {
	if previous.Goal != next.Goal || len(previous.Steps) != len(next.Steps) {
		return true
	}
	for i := range previous.Steps {
		if previous.Steps[i].Step != next.Steps[i].Step {
			return true
		}
	}
	return false
}
