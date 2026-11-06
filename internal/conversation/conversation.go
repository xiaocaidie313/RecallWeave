package conversation

import (
	"context"
	"recallweave/internal/types"
	"time"

	"gorm.io/gorm"
)

type ConversationManager struct {
	db *gorm.DB
}

func NewConversationManager(db *gorm.DB) *ConversationManager {
	return &ConversationManager{db: db}
}

func (m *ConversationManager) GetConversation(ctx context.Context, id uint) (types.Conversation, error) {
	var conversation types.Conversation
	if err := m.db.WithContext(ctx).Model(&types.Conversation{}).Where("id = ?", id).First(&conversation).Error; err != nil {
		return types.Conversation{}, err
	}
	return conversation, nil
}

func (m *ConversationManager) ConversationExists(ctx context.Context, conversationID uint) (bool, error) {
	var count int64
	err := m.db.WithContext(ctx).Model(&types.Conversation{}).Where("id = ?", conversationID).Count(&count).Error
	return count > 0, err

}

func (m *ConversationManager) ConversationTitle(ctx context.Context, conversationID uint) (string, error) {
	var item types.Conversation
	err := m.db.WithContext(ctx).Select("title").Where("id = ?", conversationID).First(&item).Error
	if err != nil {
		return "", err
	}
	return item.Title, nil
}

func (m *ConversationManager) ConversationBrief(ctx context.Context, conversationID uint) (brief string, briefUntilSeq int, err error) {
	var item types.Conversation
	err = m.db.WithContext(ctx).Select("brief", "brief_until_seq").Where("id = ?", conversationID).First(&item).Error
	if err != nil {
		return
	}
	brief = item.Brief
	briefUntilSeq = item.BriefUntilSeq
	return
}

func (m *ConversationManager) UpdateConversationBrief(ctx context.Context, conversationID uint, brief string, untilSeq int) error {
	return m.db.WithContext(ctx).Model(&types.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]any{
			"brief":           brief,
			"brief_until_seq": untilSeq,
		}).Error
}

func (m *ConversationManager) UpdateConversationTitle(ctx context.Context, conversationID uint, title string) error {
	return m.db.WithContext(ctx).Model(&types.Conversation{}).
		Where("id = ?", conversationID).
		Update("title", title).Error
}

func (m *ConversationManager) CreateConversation(ctx context.Context, title string) (types.Conversation, error) {
	item := types.Conversation{Title: title}
	err := m.db.WithContext(ctx).Create(&item).Error
	return item, err
}

func (m *ConversationManager) ListConversations(ctx context.Context, limit int) ([]types.Conversation, error) {
	var items []types.Conversation
	err := m.db.WithContext(ctx).Order("updated_at DESC, id DESC").Limit(limit).Find(&items).Error
	return items, err
}

func (m *ConversationManager) ListConversationsWithTitle(ctx context.Context, limit int) ([]struct {
	ID        uint      `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
}, error) {
	var items = []struct {
		ID        uint      `json:"id"`
		Title     string    `json:"title"`
		UpdatedAt time.Time `json:"updated_at"`
	}{}

	err := m.db.WithContext(ctx).
		Model(&types.Conversation{}).
		Select("id", "title", "updated_at").
		Order("updated_at DESC, id DESC").
		Limit(limit).
		Find(&items).Error
	if err != nil {
		return nil, err
	}
	return items, err
}
