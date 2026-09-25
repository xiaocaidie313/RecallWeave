package store

import "time"

// Session 是一段会话，也是一次导入的单位。
// SourceTag 说明它来自哪个平台，RawText 留着，方便以后换切分方式时重来。
type Session struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	ConversationID uint      `gorm:"index;not null" json:"conversation_id"`
	SourceTag      SourceTag `gorm:"size:32;index;not null" json:"source_tag"`
	Name           string    `gorm:"size:191" json:"name"`
	RawText        string    `gorm:"type:longtext" json:"raw_text"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	MessagesIDs    []uint    `gorm:"foreignKey:SessionID" json:"messages_ids"` // 由于这个session 产生的MemoryID
}

// Message 是会话里的单条消息。(SessionID, Seq) 唯一，既防重复导入，
// 也方便按区间把某条记忆的原文捞回来。
type Message struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SessionID uint      `gorm:"uniqueIndex:idx_session_seq;not null" json:"session_id"`
	Seq       int       `gorm:"uniqueIndex:idx_session_seq;not null" json:"seq"`
	Role      string    `gorm:"size:32;not null" json:"role"`
	Content   string    `gorm:"type:text;not null" json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
