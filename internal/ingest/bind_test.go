package ingest

import (
	"context"
	"errors"
	"testing"

	"recallweave/internal/parser"
	"recallweave/internal/types"
)

func TestImportIntoConversationRejectsChatSource(t *testing.T) {
	service := NewService(nil, nil)

	_, err := service.ImportIntoConversation(context.Background(), 1, types.SourceChat, "", "hello")
	if !errors.Is(err, ErrChatSource) {
		t.Fatalf("expected chat source to be rejected, got %v", err)
	}
}

func TestImportWithFormatRejectsUnknownBeforeDatabaseAccess(t *testing.T) {
	service := NewService(nil, nil)
	for _, format := range []parser.Format{"html", "auto", "MARKDOWN"} {
		t.Run(string(format), func(t *testing.T) {
			_, err := service.ImportTextWithFormat(context.Background(), types.SourceChatGPT, "", "# Notes", format)
			if !errors.Is(err, ErrInvalidFormat) {
				t.Fatalf("global import: expected invalid format, got %v", err)
			}
			_, err = service.ImportIntoConversationWithFormat(context.Background(), 1, types.SourceChatGPT, "", "# Notes", format)
			if !errors.Is(err, ErrInvalidFormat) {
				t.Fatalf("bound import: expected invalid format before querying conversation, got %v", err)
			}
		})
	}
}

func TestImportMarkdownIntoConversationStillRejectsChatSource(t *testing.T) {
	_, err := NewService(nil, nil).ImportIntoConversationWithFormat(context.Background(), 1, types.SourceChat, "", "# Notes", parser.FormatMarkdown)
	if !errors.Is(err, ErrChatSource) {
		t.Fatalf("expected reserved source to remain rejected, got %v", err)
	}
}
