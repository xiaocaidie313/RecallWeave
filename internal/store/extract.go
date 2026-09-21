package store

import (
	"time"
)

type Extract struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SourceID  uint      `gorm:"index;not null" json:"source_id"`
	MessageID uint      `gorm:"index" json:"message_id"`
	Content   string    `gorm:"type:text" json:"content"`
	SummaryID uint      `gorm:"index" json:"summary_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Summary struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Tag       string    `gorm:"index;not null" json:"tag"`
	Content   string    `gorm:"type:text" json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
