package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"recallweave/internal/parser"
	"recallweave/internal/types"
)

var (
	ErrTranscriptConflict = errors.New("transcript conflicts with saved messages")
	ErrTranscriptGap      = errors.New("transcript starts after the next expected message")
)

const (
	maxTranscriptMessages = 50
	maxTranscriptBytes    = 64 * 1024
)

type TranscriptMessage struct {
	Role    string `json:"role" jsonschema:"Original speaker: user or assistant"`
	Content string `json:"content" jsonschema:"Verbatim text of the selected message"`
}

type TranscriptOptions struct {
	ExternalSessionID string
	SeqStart          int
}

// ImportTranscript preserves the supplied roles and text.
// With an external session ID it appends only unseen messages and checks overlaps.
// It waits for extraction so a short-lived MCP process can return the outcome.
func (s *Service) ImportTranscript(ctx context.Context, sourceTag types.SourceTag, name string, transcript []TranscriptMessage, options ...TranscriptOptions) (ImportResult, error) {
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
	var opt TranscriptOptions
	if len(options) > 1 {
		return ImportResult{}, fmt.Errorf("only one transcript options value is supported")
	}
	if len(options) == 1 {
		opt = options[0]
	}
	if opt.ExternalSessionID != "" && (strings.TrimSpace(opt.ExternalSessionID) == "" || utf8.RuneCountInString(opt.ExternalSessionID) > 191) {
		return ImportResult{}, fmt.Errorf("external_session_id must contain 1 to 191 characters")
	}
	if opt.SeqStart == 0 {
		opt.SeqStart = 1
	}
	if opt.SeqStart < 1 || opt.SeqStart > math.MaxInt-len(transcript) {
		return ImportResult{}, fmt.Errorf("seq_start is outside the supported range")
	}
	if opt.ExternalSessionID == "" && opt.SeqStart != 1 {
		return ImportResult{}, fmt.Errorf("seq_start requires external_session_id")
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
			Seq: opt.SeqStart + i, Role: message.Role, Content: message.Content, SourceTag: sourceTag,
		})
	}
	if opt.ExternalSessionID != "" {
		result, err := s.syncTranscript(ctx, sourceTag, name, messages, opt)
		if err != nil {
			return ImportResult{}, err
		}
		if result.AddedMessages > 0 || result.ExtractStatus != types.ExtractDone {
			result.ExtractStatus = s.extractImport(ctx, result.SessionID)
		}
		return result, nil
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
	result.AddedMessages = len(messages)
	result.ExtractStatus = s.extractImport(ctx, result.SessionID)
	return result, nil
}
