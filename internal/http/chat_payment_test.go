package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/chat"
	"recallweave/internal/llm"
)

func TestChatReportsModelPaymentRequired(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"private provider detail"}}`)
	}))
	defer provider.Close()
	model, err := llm.NewClient(llm.Config{BaseURL: provider.URL, APIKey: "test", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	service := chat.NewChatService(agent.NewAgent(1, "test", model, "", 1), nil, nil, nil)
	router := NewRouter(nil, nil, NewChatHandler(service), nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/chat/3", strings.NewReader(`{"question":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusPaymentRequired || recorder.Body.String() != `{"error":"model provider requires payment"}` {
		t.Fatalf("unexpected chat response: %d %s", recorder.Code, recorder.Body.String())
	}
}
