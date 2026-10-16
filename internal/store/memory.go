package store

import (
	"time"

	"github.com/pgvector/pgvector-go"
)

// Memory 是从同一个会话的一段连续消息里提炼出来的一件事。
// SeqStart 到 SeqEnd 指回原文，所以回答时能说清这条记忆的依据是哪几条消息。
// 这里不存原文副本，需要时按 (SessionID, Seq 区间) 查 Message。
type Memory struct {
	ID        uint            `gorm:"primaryKey" json:"id"`
	Embedding pgvector.Vector `gorm:"type:vector(1536)" json:"embedding"`
	SessionID uint            `gorm:"index;not null" json:"session_id"`
	SeqStart  int             `gorm:"not null" json:"seq_start"`
	SeqEnd    int             `gorm:"not null" json:"seq_end"`
	Title     string          `gorm:"size:191" json:"title"`
	Content   string          `gorm:"type:text" json:"content"`
	Tag       MemoryTag       `gorm:"size:32;index" json:"tag"`
	// RecalledCount 被检索命中的次数。命中越多权重越大，是「短期记忆 → 长期记忆」的升级依据。
	RecalledCount int `gorm:"default:0" json:"recalled_count"`
	// LastAccessAt 最近一次被检索命中的时间，用于活跃度的时间衰减。
	LastAccessAt time.Time `json:"last_access_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
