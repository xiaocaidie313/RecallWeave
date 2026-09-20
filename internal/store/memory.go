package store

import "time"

// Memory 是从 Message 提炼出来的记忆。SourceID 和 MessageID 指回原文，
// 回答时才能带上来源。这一步只建表，提炼还没实现。
type Memory struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SourceID  uint      `gorm:"index;not null" json:"source_id"`
	MessageID uint      `gorm:"index" json:"message_id"`
	Title     string    `gorm:"size:191" json:"title"`
	Summary   string    `gorm:"type:text" json:"summary"`
	Tag       string    `gorm:"size:191" json:"tag"`
	Content   string    `gorm:"type:text" json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
