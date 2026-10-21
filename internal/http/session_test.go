package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestListConversationMessagesRejectsBadID(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, NewConversationHandler(nil))
	request := httptest.NewRequest(http.MethodGet, "/api/conversations/abc/messages", nil)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}
