package types

import (
	"encoding/json"
	"time"
)

// AgentRun 是一次问答运行的落库记录。State 是完整 RunState，用来复盘工具链。
type AgentRun struct {
	ID               uint            `gorm:"primaryKey" json:"id"`
	RunID            string          `gorm:"uniqueIndex;size:64;not null" json:"run_id"`
	// 委派这次run的 父ID
	ParentRunID      string          `gorm:"index;size:64" json:"parent_run_id,omitempty"`
	// 发送子任务的 子级运行ID
	SubagentID       string          `gorm:"index;size:64" json:"subagent_id,omitempty"`
	// 前一次运行ID
	PreviousRunID    string          `gorm:"index;size:64" json:"previous_run_id,omitempty"`
	ConversationID   uint            `gorm:"index" json:"conversation_id"`
	Status           string          `gorm:"size:32;index" json:"status"`
	Input            string          `gorm:"type:text" json:"input"`
	Content          string          `gorm:"type:text" json:"content"`
	// 运行状态
	State            json.RawMessage `gorm:"type:jsonb" json:"state"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	LatencyMs        int64           `json:"latency_ms"`
	Rounds           int             `json:"rounds"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}
