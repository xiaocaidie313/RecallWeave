package ingest

import (
	"context"
	"errors"
	"strings"

	"recallweave/internal/store"
)

var (
	ErrEmptyText     = errors.New("text has no importable content")
	ErrInvalidSource = errors.New("unknown source tag")
)

type IngestService struct {
	repo *IngestRepo
}

func NewIngestService(repo *IngestRepo) *IngestService {
	return &IngestService{repo: repo}
}

type ImportResult struct {
	SessionID    uint
	MessageCount int
}

func (s *IngestService) ImportText(ctx context.Context, sourceTag store.SourceTag, name, text string) (ImportResult, error) {
	if !store.IsValidSourceTag(sourceTag) {
		return ImportResult{}, ErrInvalidSource
	}

	messages := splitMessages(text)
	if len(messages) == 0 {
		return ImportResult{}, ErrEmptyText
	}

	session := &store.Session{
		SourceTag: sourceTag,
		Name:      name,
		RawText:   text,
	}
	if err := s.repo.CreateSessionWithMessages(ctx, session, messages); err != nil {
		return ImportResult{}, err
	}

	return ImportResult{SessionID: session.ID, MessageCount: len(messages)}, nil
}

// splitMessages 按空行分段，是目前最笨也最稳的切法。
// 角色暂时统一记成 user，等确定了各平台的导出格式再解析。
func splitMessages(text string) []store.Message {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")

	var messages []store.Message
	for _, block := range strings.Split(normalized, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		messages = append(messages, store.Message{
			Seq:     len(messages) + 1,
			Role:    "user",
			Content: block,
		})
	}
	return messages
}
