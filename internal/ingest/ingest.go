package ingest

import (
	"context"
	"errors"
	"strings"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

var (
	ErrEmptyText     = errors.New("text has no importable content")
	ErrInvalidSource = errors.New("unknown source tag")
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

type ImportResult struct {
	SessionID    uint
	MessageCount int
}

func (s *Service) ImportText(ctx context.Context, sourceTag store.SourceTag, name, text string) (ImportResult, error) {
	if !store.IsValidSourceTag(sourceTag) {
		return ImportResult{}, ErrInvalidSource
	}

	messages := splitMessages(text, sourceTag)
	if len(messages) == 0 {
		return ImportResult{}, ErrEmptyText
	}

	// 导入不填 ConversationID。这段原文不属于某一次助手对话。
	session := &store.Session{
		SourceTag: sourceTag,
		Name:      name,
		RawText:   text,
	}
	if err := s.createSessionWithMessages(ctx, session, messages); err != nil {
		return ImportResult{}, err
	}

	return ImportResult{SessionID: session.ID, MessageCount: len(messages)}, nil
}

// createSessionWithMessages 在一个事务里写入会话和它的消息。
// session 插入后自增 ID 才可用，所以 SessionID 在这里回填。
func (s *Service) createSessionWithMessages(ctx context.Context, session *store.Session, messages []store.Message) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(session).Error; err != nil {
			return err
		}

		if len(messages) == 0 {
			return nil
		}

		for i := range messages {
			messages[i].SessionID = session.ID
		}

		return tx.Create(&messages).Error
	})
}

// splitMessages 按空行分段，是目前最笨也最稳的切法。
// 角色暂时统一记成 user，等确定了各平台的导出格式再解析。
func splitMessages(text string, sourceTag store.SourceTag) []store.Message {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")

	var messages []store.Message
	for _, block := range strings.Split(normalized, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		messages = append(messages, store.Message{
			Seq:       len(messages) + 1,
			Role:      "user",
			SourceTag: sourceTag,
			Content:   block,
		})
	}
	return messages

	switch sourceTag{
		case store.SourceChatGPT:
		case store.SourceCursorAgent:
		case store.SourceCodex:
		case store.SourceChat:
		case store.SourceCursorIDE:
			return splitMessage_agent()
		case store.SourceWechat:
		case store.SourceQQ:
		case store.SourceTiktok:
			return splitMessage_2()
		case store.SourceWeibo:
		case store.SourceTwitter:
		case store.SourceFacebook:
		case store.SourceInstagram:
		case store.SourceYoutube:
			return splitMessage_1()
		default:
			return nil
	}
}



func dentifyTag(sourceTag store.SourceTag) {
	if 
}


// 有ai参与的对话
func splitMessage_agent(){

}
// 二人对话
func splitMessage_2(){}
// 一人对话
func splitMessage_1(){

}
