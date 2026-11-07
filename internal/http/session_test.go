package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSessionOverviewWorksWithoutConversationHandler(t *testing.T) {
	_, messages, mock := forkHTTPManagers(t)
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE .*id.*ORDER BY .*LIMIT`).WithArgs(70, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag", "name", "extract_status"}).
			AddRow(70, 7, types.SourceChat, "chat", types.ExtractDone))
	mock.ExpectQuery(`SELECT count\(\*\) FROM "messages" WHERE session_id =`).WithArgs(70).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(12))
	mock.ExpectQuery(`SELECT count\(\*\) FROM "memories" WHERE session_id =`).WithArgs(70).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	router := NewRouter(nil, nil, nil, nil, NewSessionHandler(messages, nil), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/sessions/70", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("session route requires an unrelated handler: %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		ID           uint   `json:"id"`
		SourceTag    string `json:"source_tag"`
		MessageCount int64  `json:"message_count"`
		MemoryCount  int64  `json:"memory_count"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != 70 || result.SourceTag != "chat" || result.MessageCount != 12 || result.MemoryCount != 2 {
		t.Fatalf("session overview changed: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetSessionRejectsBadID(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, NewSessionHandler(nil, nil), nil)
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/abc", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestGetRunRejectsEmptyID(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil, NewRunHandler(nil))
	request := httptest.NewRequest(http.MethodGet, "/api/runs/", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusOK {
		t.Fatalf("empty run id should not succeed, got %d", recorder.Code)
	}
}

func TestListConversationMessagesRejectsBadID(t *testing.T) {
	router := NewRouter(nil, nil, nil, NewConversationHandler(nil, nil), nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/conversations/abc/messages", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestMemoryViewsExposeContentType(t *testing.T) {
	views := memoryViews([]types.Memory{
		{ID: 1, ContentType: types.ContentPreference},
		{ID: 2}, // Existing memories without a classification remain unknown.
	})
	if len(views) != 2 || views[0].ContentType != types.ContentPreference || views[1].ContentType != types.ContentOther {
		t.Fatalf("unexpected content types: %+v", views)
	}
}
