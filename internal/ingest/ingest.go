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
	ErrEmptyText     = errors.New("text has no importable content")
	ErrInvalidSource = errors.New("unknown source tag")
	ErrUnreadable    = errors.New("unreadable import")
)

type Service struct {
	db             *gorm.DB
	extractExcuter *extract.ExtractExcuter
}

func NewService(db *gorm.DB, extractExcuter *extract.ExtractExcuter) *Service {
	return &Service{db: db, extractExcuter: extractExcuter}
}

type ImportResult struct {
	SessionID    uint
	MessageCount int
}

// ImportText 按来源把文本拆成一段或多段对话，每段各建一条 session。
func (s *Service) ImportText(ctx context.Context, sourceTag store.SourceTag, name, text string) ([]ImportResult, error) {
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
				SourceTag: sourceTag,
				Name:      sessionName,
				RawText:   text,
			}
			if err := insertSession(tx, session, conv.Messages); err != nil {
				return err
			}
			results = append(results, ImportResult{
				SessionID:    session.ID,
				MessageCount: len(conv.Messages),
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
