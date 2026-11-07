package session

import (
	"context"
	"errors"

	"recallweave/internal/types"

	"gorm.io/gorm"
)

// GetMessages 按 seq 区间把某条记忆的原文捞回来，这是「可追溯」的落点。
func (m *SessionManager) GetMessages(ctx context.Context, sessionID uint, seqStart, seqEnd int) ([]types.Message, error) {
	var messages []types.Message
	if err := m.db.WithContext(ctx).
		Where("session_id = ? AND seq >= ? AND seq <= ?", sessionID, seqStart, seqEnd).
		Where("NOT EXISTS (SELECT 1 FROM sessions AS private_session JOIN conversations AS private_conversation ON private_conversation.id = private_session.conversation_id WHERE private_session.id = messages.session_id AND private_conversation.parent_conversation_id IS NOT NULL)").
		Order("seq").
		Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// ListMessages 按顺序返回一次聊天里已经保存的消息。
func (m *SessionManager) ListMessages(ctx context.Context, sessionID uint) ([]types.Message, error) {
	var messages []types.Message
	err := m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("seq").
		Find(&messages).Error
	return messages, err
}

// AppendMessages 把新消息接在已有序号后面。
func (m *SessionManager) AppendMessages(ctx context.Context, sessionID uint, messages []types.Message) error {
	if len(messages) == 0 {
		return nil
	}

	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var last types.Message
		err := tx.Where("session_id = ?", sessionID).Order("seq desc").First(&last).Error
		seq := 0
		if err == nil {
			seq = last.Seq
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		for i := range messages {
			seq++
			messages[i].SessionID = sessionID
			messages[i].Seq = seq
		}
		return tx.Create(&messages).Error
	})
}
