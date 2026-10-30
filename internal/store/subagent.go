package store

import (
	"encoding/json"
	"time"
)

// SubagentSession is durable work context, separate from each execution Run.
// Its owning conversation is part of every storage lookup and update.
type SubagentSession struct {
	ID             string          `gorm:"primaryKey;size:64" json:"id"`
	ConversationID uint            `gorm:"index;not null" json:"conversation_id"`
	AgentName      string          `gorm:"size:40" json:"agent_name"`
	Task           string          `gorm:"type:text" json:"task"`
	LatestRunID    string          `gorm:"size:64" json:"latest_run_id"`
	Status         string          `gorm:"size:32" json:"status"`
	Snapshot       json.RawMessage `gorm:"serializer:json;type:jsonb" json:"-"`
	Preset         json.RawMessage `gorm:"serializer:json;type:jsonb" json:"-"`
	Sources        json.RawMessage `gorm:"serializer:json;type:jsonb" json:"-"`
	Revision       uint64          `gorm:"not null;default:1" json:"revision"` // 版本号 用于乐观锁 防止两个请求同时更新同一个会话
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}
