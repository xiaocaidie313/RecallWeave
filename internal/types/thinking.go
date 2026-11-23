package types

import "time"

// ThinkingCheckpoint holds the user's current place in one thinking conversation.
type ThinkingCheckpoint struct {
	ConversationID uint          `gorm:"primaryKey;autoIncrement:false" json:"conversation_id"`
	Conversation   *Conversation `gorm:"foreignKey:ConversationID;references:ID;constraint:OnDelete:CASCADE" json:"-"`
	Text           string        `gorm:"size:2000;not null" json:"text"`
	Direction      string        `gorm:"size:16;not null" json:"direction,omitempty"`
	Author         string        `gorm:"size:16;not null;check:author = 'user'" json:"author"`
	UpdatedAt      time.Time     `gorm:"not null" json:"updated_at"`
}
