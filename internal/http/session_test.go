package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/types"
)

func TestGetSessionRejectsBadID(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, NewConversationHandler(nil))
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/abc", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestGetRunRejectsEmptyID(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, NewConversationHandler(nil))
	request := httptest.NewRequest(http.MethodGet, "/api/runs/", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusOK {
		t.Fatalf("empty run id should not succeed, got %d", recorder.Code)
	}
}

func TestListConversationMessagesRejectsBadID(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, NewConversationHandler(nil))
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
