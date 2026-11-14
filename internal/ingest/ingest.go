package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"recallweave/internal/extract"
	"recallweave/internal/parser"
	"recallweave/internal/types"

	"gorm.io/gorm"
)

var (
	ErrEmptyText            = errors.New("text has no importable content")
	ErrInvalidSource        = errors.New("unknown source tag")
	ErrInvalidFormat        = errors.New("unknown import format")
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
	AddedMessages int
}

type importBind struct {
	conversationID *uint
}

// ImportText 把文本拆成会话并提炼成记忆，不挂到任何对话。
// 这些记忆留在全库里，以后靠问题相似度被捞到。
func (s *Service) ImportText(ctx context.Context, sourceTag types.SourceTag, name, text string) ([]ImportResult, error) {
	return s.ImportTextWithFormat(ctx, sourceTag, name, text, parser.FormatAuto)
}

// ImportTextWithFormat keeps source identity separate from the document format.
func (s *Service) ImportTextWithFormat(ctx context.Context, sourceTag types.SourceTag, name, text string, format parser.Format) ([]ImportResult, error) {
	return s.importText(ctx, sourceTag, name, text, format, importBind{})
}

// ImportIntoConversation 把文本挂到已有对话上，作为这次对话的 context。
func (s *Service) ImportIntoConversation(ctx context.Context, conversationID uint, sourceTag types.SourceTag, name, text string) ([]ImportResult, error) {
	return s.ImportIntoConversationWithFormat(ctx, conversationID, sourceTag, name, text, parser.FormatAuto)
}

// ImportIntoConversationWithFormat imports a document into an existing conversation.
func (s *Service) ImportIntoConversationWithFormat(ctx context.Context, conversationID uint, sourceTag types.SourceTag, name, text string, format parser.Format) ([]ImportResult, error) {
	if err := validateFormat(format); err != nil {
		return nil, err
	}
	if sourceTag == types.SourceChat {
		return nil, ErrChatSource
	}

	var count int64
	if err := s.db.WithContext(ctx).Model(&types.Conversation{}).Where("id = ?", conversationID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrConversationNotFound
	}

	id := conversationID
	return s.importText(ctx, sourceTag, name, text, format, importBind{conversationID: &id})
}

func validateFormat(format parser.Format) error {
	if !format.Valid() {
		return ErrInvalidFormat
	}
	return nil
}

func (s *Service) importText(ctx context.Context, sourceTag types.SourceTag, name, text string, format parser.Format, bind importBind) ([]ImportResult, error) {
	if err := validateFormat(format); err != nil {
		return nil, err
	}
	if !types.IsValidSourceTag(sourceTag) {
		return nil, ErrInvalidSource
	}

	conversations, err := parser.ParseWithFormat(text, sourceTag, format)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if len(conversations) == 0 {
		return nil, ErrEmptyText
	}

	results, err := s.saveConversations(ctx, sourceTag, name, text, conversations, bind)
	if err != nil {
		return nil, err
	}
	if s.extractExcuter == nil {
		for i := range results {
			results[i].ExtractStatus = s.extractImport(ctx, results[i].SessionID)
		}
		return results, nil
	}
	go func() {
		for _, result := range results {
			s.extractImport(context.Background(), result.SessionID)
		}
	}()
	return results, nil
}

func (s *Service) saveConversations(ctx context.Context, sourceTag types.SourceTag, name, text string, conversations []parser.Conversation, bind importBind) ([]ImportResult, error) {
	results := make([]ImportResult, 0, len(conversations))
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, conv := range conversations {
			sessionName := name
			if conv.Title != "" {
				sessionName = conv.Title
			}
			session := &types.Session{
				SourceTag:      sourceTag,
				Name:           sessionName,
				RawText:        text,
				ConversationID: bind.conversationID,
				ExtractStatus:  types.ExtractPending,
			}
			if err := insertSession(tx, session, conv.Messages); err != nil {
				return err
			}
			results = append(results, ImportResult{
				SessionID:     session.ID,
				MessageCount:  len(conv.Messages),
				ExtractStatus: types.ExtractPending,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Service) extractImport(ctx context.Context, sessionID uint) string {
	if s.extractExcuter == nil {
		if err := s.db.WithContext(ctx).Model(&types.Session{}).
			Where("id = ?", sessionID).
			Update("extract_status", types.ExtractSkipped).Error; err != nil {
			slog.Warn("mark extract skipped failed", "session_id", sessionID, "error", err)
		}
		return types.ExtractSkipped
	}
	if _, err := s.extractExcuter.ExtractIncrementalToMemory(ctx, sessionID); err != nil {
		slog.Warn("extract incremental failed", "session_id", sessionID, "error", err)
		if errors.Is(err, types.ErrBranchMemoryPrivate) {
			return types.ExtractSkipped
		}
		return types.ExtractFailed
	}
	return types.ExtractDone
}

// insertSession 在调用方的事务里写入会话和它的消息。
// session 插入后自增 ID 才可用，所以 SessionID 在这里回填。
func insertSession(tx *gorm.DB, session *types.Session, messages []types.Message) error {
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
