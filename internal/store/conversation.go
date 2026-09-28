package store

import "time"

// Conversation 是用户与助手之间可持续的聊天线程。
// 跟助手的 Session 通过 ConversationID 归到这条线程。导入来的会话可以不挂。
type Conversation struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	Title         string    `gorm:"size:191;not null" json:"title"`
	Brief         string    `gorm:"type:text" json:"brief"`
	BriefUntilSeq int       `json:"brief_until_seq"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
