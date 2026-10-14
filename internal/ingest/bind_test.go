package ingest

import (
	"context"
	"errors"
	"testing"

	"recallweave/internal/store"
)

func TestImportIntoConversationRejectsChatSource(t *testing.T) {
	service := NewService(nil, nil)

	_, err := service.ImportIntoConversation(context.Background(), 1, store.SourceChat, "", "hello")
	if !errors.Is(err, ErrChatSource) {
		t.Fatalf("expected chat source to be rejected, got %v", err)
	}
}
