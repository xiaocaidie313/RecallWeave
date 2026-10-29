package store

import (
	"encoding/json"
	"errors"
	"time"
)

// Conversation 是用户与助手之间可持续的聊天线程。
// 跟助手的 Session 通过 ConversationID 归到这条线程。导入来的会话可以不挂。
type Conversation struct {
	ID            uint   `gorm:"primaryKey" json:"id"`
	Title         string `gorm:"size:191;not null" json:"title"`
	Brief         string `gorm:"type:text" json:"brief"`
	BriefUntilSeq int    `json:"brief_until_seq"`
	// A fork owns a frozen copy of its inherited original messages. Brief only
	// summarizes this conversation's own later messages, never the parent's.
	ParentConversationID *uint           `gorm:"index" json:"parent_conversation_id,omitempty"`
	ForkSessionID        *uint           `json:"fork_session_id,omitempty"`
	ForkSeq              int             `json:"fork_seq,omitempty"`
	ForkSnapshot         json.RawMessage `gorm:"serializer:json;type:jsonb" json:"-"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// 传递切片  上下文消息
type ContextMessage struct {
	SessionID uint   `json:"session_id"`
	Seq       int    `json:"seq"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

func (c Conversation) IsBranch() bool { return c.ParentConversationID != nil }

// 子对话继承的父亲的 信息
func (c Conversation) InheritedMessages() ([]ContextMessage, error) {
	if !c.IsBranch() {
		return nil, nil
	}
	var messages []ContextMessage
	if err := json.Unmarshal(c.ForkSnapshot, &messages); err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, errors.New("empty fork snapshot")
	}
	for _, message := range messages {
		if message.SessionID == 0 || message.Seq <= 0 || message.Role != "user" && message.Role != "assistant" {
			return nil, errors.New("invalid original message in fork snapshot")
		}
	}
	return messages, nil
}
