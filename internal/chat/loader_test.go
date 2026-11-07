package chat

import (
	"context"
	"strings"
	"testing"

	"recallweave/internal/conversation"
	"recallweave/internal/memory"
	"recallweave/internal/session"
	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestLoadContextRequiresConversationManager(t *testing.T) {
	service := NewChatService(nil, memory.NewMemoryManger(nil, nil), session.NewSessionManager(nil), nil)
	_, _, _, _, err := service.loadContext(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "conversation manager is not configured") {
		t.Fatalf("missing conversation manager should fail before querying storage: %v", err)
	}
}

func TestLoadContextPreservesHistoryBriefAndBoundImport(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{
		DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE .*`).WithArgs(7, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "brief", "brief_until_seq"}).
			AddRow(7, "早期讨论的简报", 1))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE .*`).WithArgs(7).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag", "name"}).
			AddRow(10, 7, types.SourceChat, "chat").AddRow(12, 7, types.SourceWX, "项目原始讨论"))
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE session_id = .*`).WithArgs(12, types.MemoryActive).
		WillReturnRows(sqlmock.NewRows([]string{"id", "session_id", "seq_start", "seq_end", "tag", "title"}).
			AddRow(4, 12, 19, 30, types.TagDecision, "后端用 gin"))
	mock.ExpectQuery(`SELECT count\(\*\) FROM "conversations"`).WithArgs(7).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE .*`).WithArgs(7, types.SourceChat, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(10, 7, types.SourceChat))
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .* ORDER BY seq`).WithArgs(10).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content"}).
			AddRow(1, "user", "已压缩的历史").AddRow(2, "assistant", "近期回复").AddRow(3, "user", "近期问题"))
	service := NewChatService(nil, memory.NewMemoryManger(db, nil), session.NewSessionManager(db), nil).
		WithConversationManager(conversation.NewConversationManager(db))
	preload, sessionID, loaded, visible, err := service.loadContext(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != 10 || loaded == nil || loaded.ID != 7 || len(visible) != 3 {
		t.Fatalf("lost current conversation or source messages: session=%d conversation=%+v visible=%+v", sessionID, loaded, visible)
	}
	if len(preload.History) != 2 || preload.History[0].Role != "assistant" || preload.History[0].Content != "近期回复" ||
		preload.History[1].Role != "user" || preload.History[1].Content != "近期问题" {
		t.Fatalf("recent history should follow the brief cutoff: %+v", preload.History)
	}
	if len(preload.Background) != 2 || !strings.Contains(preload.Background[0], "项目原始讨论") ||
		!strings.Contains(preload.Background[0], "memory 4，消息 19-30") || preload.Background[1] != "此前对话简报：\n早期讨论的简报" {
		t.Fatalf("bound import catalog or conversation brief was lost: %+v", preload.Background)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFormatImportedBackground(t *testing.T) {
	got := formatImportedBackground([]importedPiece{{
		session: types.Session{ID: 12, Name: "和张三", SourceTag: types.SourceWX},
		memories: []types.Memory{{
			ID: 4, SessionID: 12, SeqStart: 19, SeqEnd: 30, Tag: types.TagDecision, Title: "后端用 gin",
		}},
	}})
	if strings.Contains(got, "近期原文") {
		t.Fatalf("bound import should stay a catalog, got %q", got)
	}
	if !strings.Contains(got, "会话 12「和张三」（wx）") || !strings.Contains(got, "memory 4，消息 19-30") {
		t.Fatalf("catalog missing memory location: %q", got)
	}

	if formatImportedBackground(nil) != "" {
		t.Fatal("empty import should not add background")
	}

	unnamed := formatImportedBackground([]importedPiece{{
		session:  types.Session{ID: 3, SourceTag: types.SourceWX},
		memories: []types.Memory{{ID: 1, Title: "一条", Tag: types.TagLife, SeqStart: 1, SeqEnd: 2}},
	}})
	if !strings.Contains(unnamed, "会话 3「未命名」（wx）") || strings.Contains(unnamed, "「wx」") {
		t.Fatalf("empty name should not reuse the source tag as a title, got %q", unnamed)
	}
}

func TestClipMemories(t *testing.T) {
	memories := make([]types.Memory, 0, 10)
	for i := 1; i <= 10; i++ {
		memories = append(memories, types.Memory{ID: uint(i), Title: "一条"})
	}

	taken, left, truncated := truncateMemories(memories, maxCatalogTotal)
	if len(taken) != maxCatalogPerSession || left != maxCatalogTotal-maxCatalogPerSession || !truncated {
		t.Fatalf("per-session cap: taken=%d left=%d truncated=%v", len(taken), left, truncated)
	}

	taken, left, truncated = truncateMemories(memories[:3], 2)
	if len(taken) != 2 || left != 0 || !truncated {
		t.Fatalf("total cap: taken=%d left=%d truncated=%v", len(taken), left, truncated)
	}
}
