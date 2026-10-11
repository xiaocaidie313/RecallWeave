// @todo muti-agent  可以写成core + ability(多个tool? 升级合并能力？)
// 目前的代码只是实现了流水线? 还不是agent
package Agent

import (
	"context"
	"errors"
	"fmt"
	"recallweave/internal/llm"
	"strings"
	"time"
	"uuid"
)

type Agent struct {
	ID            int64
	Name          string
	chatGenerator llm.ChatGenerator //开启对话
	role          string
	maxrounds     int
	tools         *llm.ToolHandle
	ChatGenerator llm.ChatGenerator

	// abilities
}
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// 运行状态  一次run 记录的状态  可变状态
type RunState struct {
	RunID          string     `json:"run_id"`
	AgentID        int64      `json:"agent_id"`
	ConversationID uint       `json:"conversation_id"`
	Input          string     `json:"input"`
	Status         RunStatus  `json:"status"`
	CurrentRound   int        `json:"current_round"`
	Steps          []RunStep  `json:"steps"`
	Errors         []RunError `json:"errors"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}
type RunError struct {
	Round    int    `json:"round"`
	ToolName string `json:"tool_name,omitempty"`
	Message  string `json:"message"`
	Fatal    bool   `json:"fatal"`
}

// 调用者传入的不可变输入
type RunRequest struct {
	Input          string
	ConversationID uint
	systemPrompt   string
}

// 返回调用者结果
type RunResult struct {
	Content string
	Rounds  int
	Steps   []RunStep
}
type RunStep struct {
	Round     int
	ToolName  string
	Arguments string
	Result    string
	Error     string
}

func NewAgent(ID int64, Name string, chatGenerator llm.ChatGenerator, role string, maxrounds int, abilities []Ability) *Agent {
	tools := llm.NewToolHandle()
	for _, ability := range abilities {
		for _, tool := range ability.Tools() {
			tools.Register(tool)
		}
	}

	return &Agent{
		ID:            ID,
		chatGenerator: chatGenerator,
		Name:          Name,
		role:          role,
		maxrounds:     maxrounds,
		tools:         tools,
	}
}

func (a *Agent) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	// 00 检查初始环境
	if a.chatGenerator == nil {
		return RunResult{}, errors.New("agent: chatGenerator is nil")
	}
	if req.Input == "" {
		return RunResult{}, errors.New("agent: input is empty")
	}
	// 01 初始化

	state := RunState{
		RunID:          uuid.New().String(),
		AgentID:        a.ID,
		ConversationID: req.ConversationID,
		Input:          req.Input,
		Status:         RunPending,
		CurrentRound:   0,
		Steps:          []RunStep{},
	}

	// 01.1 基本对话能力  1 创建conversation
	conversation := llm.NewConversation(req.systemPrompt, llm.WithConversationID(req.ConversationID))
	conversation.AddUserMessage(req.Input)
	// 02 准备 1 预加载历史上下文?
	// 03 执行
	result := RunResult{}

	for round := 1; round <= a.maxrounds; round++ {
		turn, err := a.chatGenerator.Next(ctx, conversation, a.tools.Schemas())
		if err != nil {
			return RunResult{}, errors.New("agent: chatGenerator.Next error")
		}
		result.Rounds = round
		// turn 返回这一轮 模型的响应 以及 模型想用的tool
		// conversation.AddAssistantMessage(turn.Content) // 模型响应追加到对话历史
		if len(turn.ToolCalls) == 0 {
			if strings.TrimSpace(turn.Content) == "" {
				return result, errors.New("agent: model returned empty response")
			}
			// 这一轮没有工具调用
			result.Content = turn.Content

			
			state.Status = RunCompleted
			state.FinishedAt = time.Now()
			return result, nil
		}

		for _, call := range turn.ToolCalls {
			record := RunStep{
				Round:     round,
				ToolName:  call.Name,
				Arguments: call.Arguments,
				Result:    "",
				Error:     "",
			}

			output, err := a.tools.Run(ctx, call.Name, call.Arguments)
			if err != nil {
				record.Error = err.Error()
				result.Steps = append(result.Steps, record)
				// 工具失败不立即中断，让模型决定重试或换工具。
				conversation.AddToolResult(
					call.ID,
					encodeToolError(err),
				)
				continue
			}
			record.Result = output
			result.Steps = append(result.Steps, record)

			state.CurrentRound = round
			state.Steps = append(state.Steps, record)

			conversation.AddToolResult(
				call.ID,
				output,
			)

		}
		// 04 检查
		// 05 输出结果

	}
	return result, errors.New("agent: maxrounds reached, not effective run")
}
func encodeToolError(err error) string {
	return fmt.Sprintf("tool error: %v", err)
}
