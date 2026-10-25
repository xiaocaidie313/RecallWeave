package runtime

import (
	"time"

	"recallweave/internal/plan"
	"recallweave/internal/watch/useage"
)

type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunPartial   RunStatus = "partial"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// RunState 是一次 Run 的可变状态，每次 Run 独立一份，不能放在共享的 Agent 上。
type RunState struct {
	RunID          string                  `json:"run_id"`
	AgentID        int64                   `json:"agent_id"`
	ConversationID uint                    `json:"conversation_id"`
	Input          string                  `json:"input"`
	Status         RunStatus               `json:"status"`
	CurrentRound   int                     `json:"current_round"`
	Steps          []RunStep               `json:"steps"`
	Errors         []RunError              `json:"errors"`
	Calls          []useage.ModelCallStats `json:"calls,omitempty"`
	Plan           *plan.Plan              `json:"plan,omitempty"`
	StartedAt      time.Time               `json:"started_at"`
	FinishedAt     *time.Time              `json:"finished_at,omitempty"`
}

type RunStep struct {
	Round     int    `json:"round"`
	PlanStep  string `json:"plan_step,omitempty"`
	ToolName  string `json:"tool_name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

type RunError struct {
	Round    int    `json:"round"`
	ToolName string `json:"tool_name,omitempty"`
	Message  string `json:"message"`
	Fatal    bool   `json:"fatal"`
}

// Request 是调用方传入的一次运行输入。
type Request struct {
	Input          string
	ConversationID uint
	SystemPrompt   string
	Preload        Preload
}

type RunResult struct {
	Content string   `json:"content"`
	State   RunState `json:"state"`
}
