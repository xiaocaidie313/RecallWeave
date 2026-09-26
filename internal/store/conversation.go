package store

import "time"

// Conversation 是用户与助手之间可持续的聊天线程。
// Session 继续承载导入来源或聊天消息分组，通过 ConversationID 归属到线程。
type Conversation struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Title     string    `gorm:"size:191;not null" json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
