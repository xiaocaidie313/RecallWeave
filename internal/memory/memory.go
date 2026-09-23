package memory

import (
	"context"
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

func NewMemoryManger(db *gorm.DB) *MemoryManger {
	return &MemoryManger{db: db}
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
