package ingest

import (
	"errors"
	"strings"

	"recallweave/internal/store"
)

var ErrEmptyText = errors.New("text has no importable content")

type IngestService struct {
	repo *IngestRepo
}

func NewIngestService(repo *IngestRepo) *IngestService {
	return &IngestService{repo: repo}
}

type ImportResult struct {
	SourceID     uint
	MessageCount int
}

func (s *IngestService) ImportText(sourceName, text string) (ImportResult, error) {
	messages := splitMessages(text)
	if len(messages) == 0 {
		return ImportResult{}, ErrEmptyText
	}

	source := &store.Source{Name: sourceName, RawText: text}
	if err := s.repo.CreateSourceWithMessages(source, messages); err != nil {
		return ImportResult{}, err
	}

	return ImportResult{SourceID: source.ID, MessageCount: len(messages)}, nil
}

// splitMessages 按空行分段，是目前最笨也最稳的切法。
// 角色暂时统一记成 user，等确定了导出格式再解析。
func splitMessages(text string) []store.Message {
	// normalized := strings.ReplaceAll(text, "\r\n", "\n")

	// var messages []store.Message
	// for _, block := range strings.Split(normalized, "\n\n") {
	// 	block = strings.TrimSpace(block)
	// 	if block == "" {
	// 		continue
	// 	}

	// 	messages = append(messages, store.Message{
	// 		Seq:     len(messages) + 1,
	// 		Role:    "user",
	// 		Content: block,
	// 	})
	// }

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
