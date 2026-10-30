package store

import (
	"encoding/json"
	"time"
)

// AgentRun 是一次问答运行的落库记录。State 是完整 RunState，用来复盘工具链。
type AgentRun struct {
	ID               uint            `gorm:"primaryKey" json:"id"`
	RunID            string          `gorm:"uniqueIndex;size:64;not null" json:"run_id"`
	ParentRunID      string          `gorm:"index;size:64" json:"parent_run_id,omitempty"`
	SubagentID       string          `gorm:"index;size:64" json:"subagent_id,omitempty"`
	PreviousRunID    string          `gorm:"index;size:64" json:"previous_run_id,omitempty"`
	ConversationID   uint            `gorm:"index" json:"conversation_id"`
	Status           string          `gorm:"size:32;index" json:"status"`
	Input            string          `gorm:"type:text" json:"input"`
	Content          string          `gorm:"type:text" json:"content"`
	State            json.RawMessage `gorm:"type:jsonb" json:"state"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	LatencyMs        int64           `json:"latency_ms"`
	Rounds           int             `json:"rounds"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}
