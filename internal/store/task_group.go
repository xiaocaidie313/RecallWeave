package store

import (
	"encoding/json"
	"time"
)

// TaskGroup owns a closed set of child executions for one user goal.
type TaskGroup struct {
	ID              string          `gorm:"primaryKey;size:64" json:"group_id"`
	ConversationID  uint            `gorm:"index;not null" json:"conversation_id"`
	ParentRunID     string          `gorm:"index;size:64;not null" json:"parent_run_id"`
	Goal            string          `gorm:"type:text;not null" json:"goal"`
	ExpectedOutput  string          `gorm:"type:text" json:"expected_output,omitempty"`
	DeliveryMode    string          `gorm:"size:16" json:"delivery_mode"`
	TaskIDs         json.RawMessage `gorm:"serializer:json;type:jsonb;not null" json:"task_ids"`
	Status          string          `gorm:"size:32;index;not null" json:"status"`
	CancelRequested bool            `json:"cancel_requested"`
	ReportStatus    string          `gorm:"size:16;index" json:"report_status,omitempty"`
	ReportClaim     string          `gorm:"size:64" json:"-"`
	ReportRunID     string          `gorm:"size:64" json:"report_run_id,omitempty"`
	ReportMessageID uint            `json:"report_message_id,omitempty"`
	ReportError     string          `gorm:"type:text" json:"report_error,omitempty"`
	ReportedAt      *time.Time      `json:"reported_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`// nil 表示 没有发生 如果是 time.time则会显示成 0001-01-01
}
