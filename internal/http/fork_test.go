package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/chat"
	"recallweave/internal/memory"
	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func forkHTTPManager(t *testing.T) (*memory.MemoryManger, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return memory.NewMemoryManger(db, nil), mock
}

func forkHTTPRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestForkRejectsInvalidRequestBeforeStorage(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/api/conversations/abc/fork", `{"from_seq":2}`},
		{"/api/conversations/0/fork", `{"from_seq":2}`},
		{"/api/conversations/7/fork", `{}`},
		{"/api/conversations/7/fork", `{"from_seq":0}`},
		{"/api/conversations/7/fork", `{"from_seq":-1}`},
		{"/api/conversations/7/fork", `{"from_seq":"two"}`},
		{"/api/conversations/7/fork", `not-json`},
	} {
		router := NewRouter(nil, nil, nil, NewConversationHandler(nil))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, forkHTTPRequest(tc.path, tc.body))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid request %s %s reached storage: %d %s", tc.path, tc.body, recorder.Code, recorder.Body.String())
		}
	}
}

func expectHTTPForkSource(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE .*id.*ORDER BY .*LIMIT`).WithArgs(7, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "brief", "brief_until_seq"}).AddRow(7, "主线", "不能继承的未来摘要", 100))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE conversation_id = .*source_tag = .*ORDER BY .*LIMIT`).
		WithArgs(7, store.SourceChat, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(70, 7, "chat"))
}

func TestForkCreatesBranchAndReturnsMetadata(t *testing.T) {
	manager, mock := forkHTTPManager(t)
	mock.ExpectBegin()
	expectHTTPForkSource(mock)
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*seq <= .*ORDER BY seq LIMIT`).
		WithArgs(70, 2, memory.MaxForkMessages+1).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content"}).AddRow(1, "user", "原问题").AddRow(2, "assistant", "原建议"))
	mock.ExpectQuery(`INSERT INTO "conversations"`).
		WithArgs("选型分支", "", 0, 7, 70, 2, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(8))
	mock.ExpectQuery(`INSERT INTO "sessions"`).
		WithArgs(8, store.SourceChat, "chat", "", store.ExtractSkipped, "", nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(80))
	mock.ExpectCommit()
	router := NewRouter(nil, nil, nil, NewConversationHandler(manager))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, forkHTTPRequest("/api/conversations/7/fork", `{"from_seq":2,"title":"选型分支"}`))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected created branch, got %d %s", recorder.Code, recorder.Body.String())
	}
	var result memory.ForkResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Conversation.ID != 8 || result.SessionID != 80 || result.InheritedCount != 2 ||
		result.Conversation.ParentConversationID == nil || *result.Conversation.ParentConversationID != 7 ||
		result.Conversation.ForkSessionID == nil || *result.Conversation.ForkSessionID != 70 || result.Conversation.ForkSeq != 2 ||
		result.Conversation.Brief != "" || result.Conversation.BriefUntilSeq != 0 {
		t.Fatalf("invalid branch metadata: %+v", result)
	}
	if strings.Contains(recorder.Body.String(), "原问题") || strings.Contains(recorder.Body.String(), "未来摘要") {
		t.Fatal("create response must not serialize the private snapshot or parent summary")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestForkStorageErrorsMapToHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{"missing_parent", http.StatusNotFound},
		{"missing_cutoff", http.StatusBadRequest},
		{"oversized_snapshot", http.StatusRequestEntityTooLarge},
		{"database_error", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, mock := forkHTTPManager(t)
			mock.ExpectBegin()
			switch tc.name {
			case "missing_parent":
				mock.ExpectQuery(`SELECT .* FROM "conversations"`).WithArgs(7, 1).
					WillReturnRows(sqlmock.NewRows([]string{"id"}))
			case "database_error":
				mock.ExpectQuery(`SELECT .* FROM "conversations"`).WithArgs(7, 1).
					WillReturnError(errors.New("test database unavailable"))
			default:
				expectHTTPForkSource(mock)
				rows := sqlmock.NewRows([]string{"seq", "role", "content"})
				if tc.name == "oversized_snapshot" {
					rows.AddRow(2, "user", strings.Repeat("x", memory.MaxForkSnapshotBytes))
				}
				mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*seq <= .*ORDER BY seq LIMIT`).
					WithArgs(70, 2, memory.MaxForkMessages+1).WillReturnRows(rows)
			}
			mock.ExpectRollback()
			router := NewRouter(nil, nil, nil, NewConversationHandler(manager))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, forkHTTPRequest("/api/conversations/7/fork", `{"from_seq":2}`))
			if recorder.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChatRouteUsesRenamedService(t *testing.T) {
	service := chat.NewAgent(agent.NewAgent(1, "test", nil, "", 3), nil, nil)
	router := NewRouter(nil, nil, NewChatHandler(service), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, forkHTTPRequest("/api/chat/1", `{"question":"你好"}`))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "llm is not configured") {
		t.Fatalf("renamed chat route/service not connected: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestBranchHistoryKeepsInheritedSourceLocations(t *testing.T) {
	manager, mock := forkHTTPManager(t)
	inherited := []store.ContextMessage{{SessionID: 70, Seq: 2, Role: "assistant", Content: "冻结的父会话原文"}}
	snapshot, err := json.Marshal(inherited)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE .*id.*ORDER BY .*LIMIT`).WithArgs(8, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_conversation_id", "fork_snapshot"}).AddRow(8, 7, snapshot))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE conversation_id = .*source_tag = .*ORDER BY .*LIMIT`).
		WithArgs(8, store.SourceChat, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(80, 8, "chat"))
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*ORDER BY seq`).WithArgs(80).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content"}).AddRow(1, "user", "支线自己的新消息"))
	router := NewRouter(nil, nil, nil, NewConversationHandler(manager))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/conversations/8/messages", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("failed to load branch history: %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		SessionID uint                   `json:"session_id"`
		Inherited []store.ContextMessage `json:"inherited_messages"`
		Messages  []struct {
			Seq     int    `json:"seq"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SessionID != 80 || len(result.Inherited) != 1 || result.Inherited[0] != inherited[0] ||
		len(result.Messages) != 1 || result.Messages[0].Seq != 1 || result.Messages[0].Content != "支线自己的新消息" {
		t.Fatalf("inherited provenance mixed into branch message numbering: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
