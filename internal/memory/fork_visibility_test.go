package memory

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGlobalSourceToolsExcludeBranches(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	// Global Agent/MCP readers must not regain a branch's private messages by
	// guessing its session/conversation IDs. Explicit UI history uses ListMessages.
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE .*NOT EXISTS .*private_session.id = messages.session_id AND private_conversation.parent_conversation_id IS NOT NULL`).
		WithArgs(12, 1, 50).WillReturnRows(sqlmock.NewRows([]string{"seq", "content"}))
	got, err := manager.GetMessages(context.Background(), 12, 1, 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("global source read: %+v, %v", got, err)
	}
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE .*NOT EXISTS .*private_conversation.id = sessions.conversation_id AND private_conversation.parent_conversation_id IS NOT NULL`).
		WithArgs(3).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	sessions, err := manager.GetSessions(context.Background(), 3)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("global session discovery: %+v, %v", sessions, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
