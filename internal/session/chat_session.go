package session

import (
	"context"
	"errors"
	"fmt"

	"recallweave/internal/types"

	"gorm.io/gorm"
)

// ChatSession 是跟助手的这一次聊天。同一个 conversation 只建一条，
// 导入的其他来源会话不放进来。
func (m *SessionManager) ChatSession(ctx context.Context, conversationID uint) (*types.Session, error) {
	var count int64
	err := m.db.WithContext(ctx).Model(&types.Conversation{}).Where("id = ?", conversationID).Count(&count).Error
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	var session types.Session
	err = m.db.WithContext(ctx).
		//默认配置成 SourceChat 的 session 聊天会话
		Where("conversation_id = ? AND source_tag = ?", conversationID, types.SourceChat).
		First(&session).Error
	if err == nil {
		return &session, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	// 没找到就新建
	session = types.Session{
		ConversationID: &conversationID,
		SourceTag:      types.SourceChat,
		Name:           "chat",
		ExtractStatus:  types.ExtractPending,
	}
	if err := m.db.WithContext(ctx).Create(&session).Error; err != nil {
		return nil, fmt.Errorf("create chat session failed: %w", err)
	}
	return &session, nil
}

func (m *SessionManager) FindChatSession(ctx context.Context, conversationID uint) (*types.Session, error) {
	var session types.Session
	err := m.db.WithContext(ctx).
		Where("conversation_id = ? AND source_tag = ?", conversationID, types.SourceChat).
		First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}
