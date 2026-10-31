package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
)

const FollowupTaskToolName = "followup_task"
const ListSubagentsToolName = "list_subagents"

const continuationInstructions = `可用 followup_task 向同一个子任务补充要求。subagent_id 是持续会话身份，child_run_id 是其中一次执行；每次补查会产生新的 Run。
使用本轮委派结果中的 subagent_id，或先用 list_subagents 查找当前对话已有的子任务。不要猜 ID，也不能续接其他对话的子任务。
补查默认同步等待；每个主 Run 最多新委派一次、补查一次，两项分别限制。补查由用户补充要求、子任务返回的问题或对照交付要求发现的缺口触发，不必每次都补查。
只有 can_follow_up=true 才表示上下文已保存且可以尝试续接。completed 只说明本次执行正常结束，不保证任务已经充分完成或结论正确。`

type FollowupRequest struct {
	SubagentID string `json:"subagent_id"`
	Message    string `json:"message"`
	Background bool   `json:"background,omitempty"`
}

type SubagentInfo struct {
	TaskID      string    `json:"task_id,omitempty"`
	SubagentID  string    `json:"subagent_id"`
	Agent       string    `json:"agent"`
	Task        string    `json:"task"`
	LastRunID   string    `json:"last_run_id"`
	Status      RunStatus `json:"status"`
	CanFollowUp bool      `json:"can_follow_up"`
}

// ContinuationDelegator is optional. Simple one-shot Delegators keep their
// original contract and do not expose persistence-dependent tools.
type ContinuationDelegator interface {
	ContinuationsEnabled() bool
	FollowUp(context.Context, DelegationParent, FollowupRequest) (RunResult, error)
	ListSubagents(context.Context, DelegationParent) ([]SubagentInfo, error)
}

func continuationTools(state *RunState, parent *agent.Agent, continuation ContinuationDelegator) []llm.Tool {
	used := false
	background, backgroundEnabled := enabledBackground(continuation)
	followupProperties := map[string]any{
		"subagent_id": map[string]any{"type": "string", "description": "委派或 list_subagents 返回的子任务 ID，32 位小写十六进制"},
		"message":     map[string]any{"type": "string", "description": "补充要求及当前约束，非空且最多 8 KiB"},
	}
	description := "向当前对话中同一个子任务补充要求，恢复它的消息和工具历史后同步执行。每个主 Run 最多一次实际补查。"
	if backgroundEnabled {
		followupProperties["background"] = map[string]any{"type": "boolean", "default": false, "description": "true 后台续接并立刻返回任务 ID；省略时同步等待"}
		description = "向同一个子任务补充要求并恢复其历史。默认同步；background=true 提交后台续接。每个主 Run 最多一次，后台与同步共用额度。"
	}
	parentInfo := func() DelegationParent {
		return DelegationParent{RunID: state.RunID, ConversationID: state.ConversationID, Agent: parent}
	}
	return []llm.Tool{
		{
			Schema: llm.ToolSchema{
				Name:        FollowupTaskToolName,
				Description: description,
				Parameters: map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": followupProperties,
					"required":   []string{"subagent_id", "message"},
				},
			},
			Run: func(ctx context.Context, arguments string) (string, error) {
				request, err := decodeFollowup(arguments)
				if err != nil {
					return "", err
				}
				if used {
					return "", fmt.Errorf("followup_task: this Run already used its one follow-up")
				}
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if request.Background {
					if !backgroundEnabled {
						return "", fmt.Errorf("followup_task: background execution is unavailable")
					}
					task, submitErr := background.SubmitFollowUp(ctx, parentInfo(), request)
					if task.TaskID == "" {
						if submitErr != nil {
							return "", submitErr
						}
						return "", fmt.Errorf("followup_task: background submission did not produce a task")
					}
					used = true
					return encodeBackgroundTask(task, submitErr)
				}
				child, runErr := continuation.FollowUp(ctx, parentInfo(), request)
				if child.State.RunID == "" {
					if runErr != nil {
						return "", runErr
					}
					return "", fmt.Errorf("followup_task: child did not produce a Run")
				}
				used = true
				state.Children = append(state.Children, child)
				return encodeDelegationResult(child, runErr)
			},
		},
		{
			Schema: llm.ToolSchema{
				Name:        ListSubagentsToolName,
				Description: "列出当前对话已有的子任务 ID、角色、任务和最近状态，以便选择同一任务补查。",
				Parameters:  map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
			},
			Run: func(ctx context.Context, arguments string) (string, error) {
				if strings.TrimSpace(arguments) == "" {
					arguments = "{}"
				}
				var empty struct{}
				if err := decodeContinuationJSON(arguments, 1024, &empty); err != nil {
					return "", fmt.Errorf("list_subagents: %w", err)
				}
				if err := ctx.Err(); err != nil {
					return "", err
				}
				items, err := continuation.ListSubagents(ctx, parentInfo())
				if err != nil {
					return "", err
				}
				if items == nil {
					items = []SubagentInfo{}
				}
				encoded, err := json.Marshal(struct {
					Subagents []SubagentInfo `json:"subagents"`
				}{items})
				return string(encoded), err
			},
		},
	}
}

func decodeFollowup(arguments string) (FollowupRequest, error) {
	var request FollowupRequest
	if err := decodeContinuationJSON(arguments, 16<<10, &request); err != nil {
		return request, fmt.Errorf("followup_task: %w", err)
	}
	if len(request.SubagentID) != 32 || strings.ToLower(request.SubagentID) != request.SubagentID {
		return request, fmt.Errorf("followup_task: subagent_id must be 32 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(request.SubagentID); err != nil {
		return request, fmt.Errorf("followup_task: invalid subagent_id")
	}
	if strings.TrimSpace(request.Message) == "" || len(request.Message) > 8<<10 {
		return request, fmt.Errorf("followup_task: message must be nonempty and at most 8 KiB")
	}
	return request, nil
}

func decodeContinuationJSON(arguments string, limit int, target any) error {
	trimmed := strings.TrimSpace(arguments)
	if len(arguments) > limit || !utf8.ValidString(arguments) || !strings.HasPrefix(trimmed, "{") {
		return fmt.Errorf("expected a bounded UTF-8 JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON object")
	}
	return nil
}
