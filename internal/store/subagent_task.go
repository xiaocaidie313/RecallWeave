package store

import (
	"encoding/json"
	"time"
)

// 子agent 任务记录
type SubagentTask struct {
	ID              string          `gorm:"primaryKey;size:64" json:"task_id"`
	SubagentID      string          `gorm:"index;size:64;not null" json:"subagent_id"`
	ParentRunID     string          `gorm:"index;size:64" json:"parent_run_id"`
	RunID           string          `gorm:"index;size:64" json:"run_id,omitempty"`
	PreviousRunID   string          `gorm:"size:64" json:"previous_run_id,omitempty"`
	ConversationID  uint            `gorm:"index;not null" json:"conversation_id"`
	AgentName       string          `gorm:"size:40;not null" json:"agent"`
	Task            string          `gorm:"type:text;not null" json:"task"`
	Status          string          `gorm:"size:32;index;not null" json:"status"`
	CancelRequested bool            `json:"cancel_requested"`
	Result          json.RawMessage `gorm:"serializer:json;type:jsonb" json:"-"`
	Error           string          `gorm:"type:text" json:"error,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}
