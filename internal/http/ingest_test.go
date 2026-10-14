package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImportConversationRejectsBadID(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/abc/imports", strings.NewReader(`{"source_tag":"wx","text":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestImportConversationRejectsMissingText(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/1/imports", strings.NewReader(`{"source_tag":"wx"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}
