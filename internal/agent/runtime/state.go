package runtime

import (
	"time"

	"recallweave/internal/llm"
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
	RunID                string                    `json:"run_id"`
	ParentRunID          string                    `json:"parent_run_id,omitempty"`
	SubagentID           string                    `json:"subagent_id,omitempty"`
	PreviousRunID        string                    `json:"previous_run_id,omitempty"`
	AgentID              int64                     `json:"agent_id"`
	AgentName            string                    `json:"agent_name,omitempty"`
	ConversationID       uint                      `json:"conversation_id"`
	Input                string                    `json:"input"`
	Status               RunStatus                 `json:"status"`
	CurrentRound         int                       `json:"current_round"`
	Steps                []RunStep                 `json:"steps"`
	Errors               []RunError                `json:"errors"`
	Calls                []useage.ModelCallStats   `json:"calls,omitempty"`
	Children             []RunResult               `json:"children,omitempty"`
	ContextSnapshot      *llm.ConversationSnapshot `json:"context_snapshot,omitempty"`
	ContextError         string                    `json:"context_error,omitempty"`
	ContextPersistFailed bool                      `json:"context_persist_failed,omitempty"`
	InheritedSources     []Source                  `json:"inherited_sources,omitempty"`
	Plan                 *plan.Plan                `json:"plan,omitempty"`
	StartedAt            time.Time                 `json:"started_at"`
	FinishedAt           *time.Time                `json:"finished_at,omitempty"`
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
	Input            string
	ParentRunID      string
	SubagentID       string
	PreviousRunID    string
	ConversationID   uint
	SystemPrompt     string
	Preload          Preload
	Delegator        Delegator
	ContextSnapshot  *llm.ConversationSnapshot
	CaptureContext   bool
	InheritedSources []Source
}

type RunResult struct {
	Content string   `json:"content"`
	State   RunState `json:"state"`
}

// AllCalls includes child calls without changing Calls, which always belongs
// to this Run alone. Failed child Runs still contribute their actual usage.
func (s RunState) AllCalls() []useage.ModelCallStats {
	calls := append([]useage.ModelCallStats(nil), s.Calls...)
	for _, child := range s.Children {
		calls = append(calls, child.State.AllCalls()...)
	}
	return calls
}

// AllSteps makes successfully retrieved child evidence available to the same
// citation checks used for a parent's own tools.
func (s RunState) AllSteps() []RunStep {
	steps := append([]RunStep(nil), s.Steps...)
	for _, child := range s.Children {
		steps = append(steps, child.State.AllSteps()...)
	}
	return steps
}
