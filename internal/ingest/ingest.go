package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"recallweave/internal/extract"
	"recallweave/internal/parser"
	"recallweave/internal/store"

	"gorm.io/gorm"
)

var (
	ErrEmptyText            = errors.New("text has no importable content")
	ErrInvalidSource        = errors.New("unknown source tag")
	ErrUnreadable           = errors.New("unreadable import")
	ErrConversationNotFound = errors.New("conversation not found")
	ErrChatSource           = errors.New("source_tag chat is reserved")
)

type Service struct {
	db             *gorm.DB
	extractExcuter *extract.ExtractExcuter
}

func NewService(db *gorm.DB, extractExcuter *extract.ExtractExcuter) *Service {
	return &Service{db: db, extractExcuter: extractExcuter}
}

type ImportResult struct {
	SessionID     uint
	MessageCount  int
	ExtractStatus string
}

type importBind struct {
	conversationID *uint
}

// ImportText 把文本拆成会话并提炼成记忆，不挂到任何对话。
// 这些记忆留在全库里，以后靠问题相似度被捞到。
func (s *Service) ImportText(ctx context.Context, sourceTag store.SourceTag, name, text string) ([]ImportResult, error) {
	return s.importText(ctx, sourceTag, name, text, importBind{})
}

// ImportIntoConversation 把文本挂到已有对话上，作为这次对话的 context。
func (s *Service) ImportIntoConversation(ctx context.Context, conversationID uint, sourceTag store.SourceTag, name, text string) ([]ImportResult, error) {
	if sourceTag == store.SourceChat {
		return nil, ErrChatSource
	}

	var count int64
	if err := s.db.WithContext(ctx).Model(&store.Conversation{}).Where("id = ?", conversationID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrConversationNotFound
	}

	id := conversationID
	return s.importText(ctx, sourceTag, name, text, importBind{conversationID: &id})
}

func (s *Service) importText(ctx context.Context, sourceTag store.SourceTag, name, text string, bind importBind) ([]ImportResult, error) {
	if !store.IsValidSourceTag(sourceTag) {
		return nil, ErrInvalidSource
	}

	conversations, err := parser.Parse(text, sourceTag)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if len(conversations) == 0 {
		return nil, ErrEmptyText
	}

	results := make([]ImportResult, 0, len(conversations))
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, conv := range conversations {
			sessionName := name
			if conv.Title != "" {
				sessionName = conv.Title
			}
			session := &store.Session{
				SourceTag:      sourceTag,
				Name:           sessionName,
				RawText:        text,
				ConversationID: bind.conversationID,
				ExtractStatus:  store.ExtractPending,
			}
			if err := insertSession(tx, session, conv.Messages); err != nil {
				return err
			}
			results = append(results, ImportResult{
				SessionID:     session.ID,
				MessageCount:  len(conv.Messages),
				ExtractStatus: store.ExtractPending,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 说明写入Session成功了 ---》 自动做摘要
	// 没配 LLM（extractExcuter 为 nil）时不提炼，只入库。
	if s.extractExcuter == nil {
		for i := range results {
			results[i].ExtractStatus = store.ExtractSkipped
			if err := s.db.WithContext(ctx).Model(&store.Session{}).
				Where("id = ?", results[i].SessionID).
				Update("extract_status", store.ExtractSkipped).Error; err != nil {
				slog.Warn("mark extract skipped failed", "session_id", results[i].SessionID, "error", err)
			}
		}
		return results, nil
	}
	go func() {
		ctx := context.Background()
		for _, result := range results {
			_, err := s.extractExcuter.ExtractNewToMemory(ctx, result.SessionID)
			if err != nil {
				slog.Warn("extract new failed", "session_id", result.SessionID, "error", err)
			}
		}
	}()

	return results, nil
}

// insertSession 在调用方的事务里写入会话和它的消息。
// session 插入后自增 ID 才可用，所以 SessionID 在这里回填。
func insertSession(tx *gorm.DB, session *store.Session, messages []store.Message) error {
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
}
