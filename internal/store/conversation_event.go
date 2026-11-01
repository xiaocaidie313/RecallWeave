package store

import (
	"encoding/json"
	"time"
)

type ConversationEvent struct {
	ID             uint64          `gorm:"primaryKey;autoIncrement" json:"id"`
	ConversationID uint            `gorm:"index;uniqueIndex:idx_conversation_event_key;not null" json:"conversation_id"`
	Kind           string          `gorm:"size:64;not null" json:"kind"`
	Key            string          `gorm:"size:191;uniqueIndex:idx_conversation_event_key;not null" json:"-"`
	Data           json.RawMessage `gorm:"serializer:json;type:jsonb;not null" json:"data"`
	CreatedAt      time.Time       `json:"created_at"`
}
