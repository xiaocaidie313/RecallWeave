package store

import "time"

// Memory 是从同一个会话的一段连续消息里提炼出来的一件事。
// SeqStart 到 SeqEnd 指回原文，所以回答时能说清这条记忆的依据是哪几条消息。
// 这里不存原文副本，需要时按 (SessionID, Seq 区间) 查 Message。
type Memory struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Embedding []float64 `gorm:"serializer:json" json:"embedding"`
	SessionID uint      `gorm:"index;not null" json:"session_id"`
	SeqStart  int       `gorm:"not null" json:"seq_start"`
	SeqEnd    int       `gorm:"not null" json:"seq_end"`
	Title     string    `gorm:"size:191" json:"title"`
	Content   string    `gorm:"type:text" json:"content"`
	Tag       MemoryTag `gorm:"size:32;index" json:"tag"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
