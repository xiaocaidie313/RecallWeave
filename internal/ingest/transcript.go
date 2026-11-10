package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/parser"
	"recallweave/internal/types"
)

const (
	maxTranscriptMessages = 50
	maxTranscriptBytes    = 64 * 1024
)

type TranscriptMessage struct {
	Role    string `json:"role" jsonschema:"Original speaker: user or assistant"`
	Content string `json:"content" jsonschema:"Verbatim text of the selected message"`
}

// ImportTranscript preserves the supplied roles and text in a new imported session.
// It waits for extraction so a short-lived MCP process can return the outcome.
func (s *Service) ImportTranscript(ctx context.Context, sourceTag types.SourceTag, name string, transcript []TranscriptMessage) (ImportResult, error) {
	if sourceTag == types.SourceChat {
		return ImportResult{}, ErrChatSource
	}
	if !types.IsValidSourceTag(sourceTag) {
		return ImportResult{}, ErrInvalidSource
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 191 {
		return ImportResult{}, fmt.Errorf("name must contain 1 to 191 characters")
	}
	if len(transcript) == 0 || len(transcript) > maxTranscriptMessages {
		return ImportResult{}, fmt.Errorf("transcript must contain 1 to %d messages", maxTranscriptMessages)
	}
	messages := make([]types.Message, 0, len(transcript))
	totalBytes := 0
	for i, message := range transcript {
		if message.Role != "user" && message.Role != "assistant" {
			return ImportResult{}, fmt.Errorf("message %d: role must be user or assistant", i+1)
		}
		if strings.TrimSpace(message.Content) == "" {
			return ImportResult{}, fmt.Errorf("message %d: content is empty", i+1)
		}
		totalBytes += len(message.Content)
		if totalBytes > maxTranscriptBytes {
			return ImportResult{}, fmt.Errorf("transcript text exceeds %d bytes", maxTranscriptBytes)
		}
		messages = append(messages, types.Message{
			Seq: i + 1, Role: message.Role, Content: message.Content, SourceTag: sourceTag,
		})
	}
	raw, err := json.Marshal(transcript)
	if err != nil {
		return ImportResult{}, err
	}
	results, err := s.saveConversations(ctx, sourceTag, name, string(raw), []parser.Conversation{{Messages: messages}}, importBind{})
	if err != nil {
		return ImportResult{}, err
	}
	result := results[0]
	result.ExtractStatus = s.extractImport(ctx, result.SessionID)
	return result, nil
}
