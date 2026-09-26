package memory

import (
	"context"
	"errors"
	"strings"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20
)

type MemoryManger struct {
	db *gorm.DB
}

// 预加载历史对话的 总计的清单 让llm 不重新 根据全部历史记录 重新生成 而是 直接从清单里 提取 记忆
type MemoryBriefParam struct {
	ConversationID uint   `json:"conversation_id"`
	SessionID      []uint `json:"session_id"`
	// Brief string `json:"brief"`
	// raw ? 如果太多 ?
}

func NewMemoryManger(db *gorm.DB) *MemoryManger {
	return &MemoryManger{db: db}
}

func (m *MemoryManger) ConversationExists(ctx context.Context, conversationID uint) (bool, error) {
	var count int64
	err := m.db.WithContext(ctx).Model(&store.Conversation{}).Where("id = ?", conversationID).Count(&count).Error
	return count > 0, err
}

func (m *MemoryManger) ConversationTitle(ctx context.Context, conversationID uint) (string, error) {
	var item store.Conversation
	err := m.db.WithContext(ctx).Select("title").First(&item, conversationID).Error
	if err != nil {
		return "", err
	}
	return item.Title, nil
}

func (m *MemoryManger) UpdateConversationTitle(ctx context.Context, conversationID uint, title string) error {
	return m.db.WithContext(ctx).Model(&store.Conversation{}).
		Where("id = ?", conversationID).
		Update("title", title).Error
}

func (m *MemoryManger) SearchMemories(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	// 只是词的匹配
	query := m.db.WithContext(ctx).Model(&store.Memory{})
	if tokens := strings.Fields(keyword); len(tokens) > 0 {
		conditions := m.db.Session(&gorm.Session{NewDB: true})
		for _, token := range tokens {
			like := "%" + token + "%"
			conditions = conditions.Or("title LIKE ? OR content LIKE ?", like, like)
		}
		query = query.Where(conditions)
	}
	if tag != "" {
		query = query.Where("tag = ?", tag)
	}

	var memories []store.Memory
	if err := query.Order("id DESC").Limit(limit).Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

// ListBySession 返回一个会话下的记忆，按它在原文里的起始位置排序。
func (m *MemoryManger) ListBySession(ctx context.Context, sessionID uint) ([]store.Memory, error) {
	var memories []store.Memory
	if err := m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("seq_start").
		Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

// GetMessages 按 seq 区间把某条记忆的原文捞回来，这是「可追溯」的落点。
func (m *MemoryManger) GetMessages(ctx context.Context, sessionID uint, seqStart, seqEnd int) ([]store.Message, error) {
	var messages []store.Message
	if err := m.db.WithContext(ctx).
		Where("session_id = ? AND seq >= ? AND seq <= ?", sessionID, seqStart, seqEnd).
		Order("seq").
		Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// ChatSession 是跟助手的这一次聊天。同一个 conversation 只建一条，
// 导入的其他来源会话不放进来。
func (m *MemoryManger) ChatSession(ctx context.Context, conversationID uint) (*store.Session, error) {
	exists, err := m.ConversationExists(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gorm.ErrRecordNotFound
	}

	var session store.Session
	err = m.db.WithContext(ctx).
		Where("conversation_id = ? AND source_tag = ?", conversationID, store.SourceChat).
		First(&session).Error
	if err == nil {
		return &session, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	session = store.Session{
		ConversationID: conversationID,
		SourceTag:      store.SourceChat,
		Name:           "chat",
	}
	if err := m.db.WithContext(ctx).Create(&session).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

// ListMessages 按顺序返回一次聊天里已经保存的消息。
func (m *MemoryManger) ListMessages(ctx context.Context, sessionID uint) ([]store.Message, error) {
	var messages []store.Message
	err := m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("seq").
		Find(&messages).Error
	return messages, err
}

// AppendMessages 把新消息接在已有序号后面。
func (m *MemoryManger) AppendMessages(ctx context.Context, sessionID uint, messages []store.Message) error {
	if len(messages) == 0 {
		return nil
	}

	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var last store.Message
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

func (m *MemoryManger) GetSessions(ctx context.Context, conversationID uint) ([]store.Session, error) {
	var sessions []store.Session
	if err := m.db.WithContext(ctx).Model(&store.Session{}).Where("conversation_id = ?", conversationID).Find(&sessions).Error; err != nil {
		return nil, err
	}
	return sessions, nil
}

// 根据conversationID 找 session 找 memmory 然后汇总
func (m *MemoryManger) GetMemoryBrief(ctx context.Context, conversationID uint) (MemoryBriefParam, error) {
	var relatedSessions []store.Session
	if err := m.db.WithContext(ctx).Model(&store.Session{}).Where("conversation_id = ?", conversationID).Find(&relatedSessions).Error; err != nil {
		return MemoryBriefParam{}, err
	}
	var sessionIDs []uint
	for _, session := range relatedSessions {
		sessionIDs = append(sessionIDs, session.ID)
	}
	return MemoryBriefParam{
		ConversationID: conversationID,
		SessionID:      sessionIDs,
	}, nil
}
