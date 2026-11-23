package session

import (
	"context"

	"recallweave/internal/types"

	"gorm.io/gorm"
)

// SessionManager owns transcript storage and message provenance. Memory retrieval
// and lifecycle operations remain in the memory package.
type SessionManager struct {
	db *gorm.DB
}

func NewSessionManager(db *gorm.DB) *SessionManager {
	return &SessionManager{db: db}
}

type LocalNote struct {
	SessionID     uint   `json:"session_id"`
	Name          string `json:"name"`
	ExtractStatus string `json:"extract_status"`
}

func (m *SessionManager) ListLocalNotes(ctx context.Context, conversationID uint) ([]LocalNote, error) {
	notes := []LocalNote{}
	err := m.db.WithContext(ctx).Model(&types.Session{}).
		Select("id AS session_id, name, extract_status").
		Where("conversation_id = ? AND source_tag = ?", conversationID, types.SourceLocalNote).
		Order("created_at DESC, id DESC").Find(&notes).Error
	return notes, err
}

func (m *SessionManager) GetSessions(ctx context.Context, conversationID uint) ([]types.Session, error) {
	var sessions []types.Session
	if err := m.db.WithContext(ctx).Model(&types.Session{}).Where("conversation_id = ?", conversationID).
		Where("NOT EXISTS (SELECT 1 FROM conversations AS private_conversation WHERE private_conversation.id = sessions.conversation_id AND private_conversation.parent_conversation_id IS NOT NULL)").Find(&sessions).Error; err != nil {
		return nil, err
	}
	return sessions, nil
}

type SessionOverview struct {
	Session      types.Session
	MessageCount int64
	MemoryCount  int64
}

func (m *SessionManager) GetSession(ctx context.Context, sessionID uint) (types.Session, error) {
	var session types.Session
	err := m.db.WithContext(ctx).First(&session, sessionID).Error
	return session, err
}

func (m *SessionManager) SessionOverview(ctx context.Context, sessionID uint) (SessionOverview, error) {
	session, err := m.GetSession(ctx, sessionID)
	if err != nil {
		return SessionOverview{}, err
	}

	var messages, memories int64
	if err := m.db.WithContext(ctx).Model(&types.Message{}).Where("session_id = ?", sessionID).Count(&messages).Error; err != nil {
		return SessionOverview{}, err
	}
	if err := m.db.WithContext(ctx).Model(&types.Memory{}).Where("session_id = ?", sessionID).Count(&memories).Error; err != nil {
		return SessionOverview{}, err
	}
	return SessionOverview{
		Session:      session,
		MessageCount: messages,
		MemoryCount:  memories,
	}, nil
}
