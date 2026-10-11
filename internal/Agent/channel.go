package Agent

import (
	"encoding/json"
	"time"
)

// agent 间传递信息的schema

type Message struct {
	ID          int64           `json:"id"`
	FromAgentID string          `json:"from_agent_id"`
	ToAgentID   string          `json:"to_agent_id"`
	TaskID      string          `json:"task_id"`
	Content     string          `json:"content"`
	Topic       string          `json:"topic"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type worker struct {
	ID  string
	msg Message
}
