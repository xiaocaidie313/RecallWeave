package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recallweave/internal/chat"
	"recallweave/internal/thinking"
)

func TestThinkingHTTPRejectsInvalidRequestsBeforeModelOrStorage(t *testing.T) {
	handler := NewChatHandler(chat.NewChatService(nil, nil, nil, nil))
	router := NewRouter(nil, nil, handler, nil, nil, nil)
	MountThinkingCheckpoints(router, thinking.NewStore(nil))
	const chatPath = "/api/chat/7"
	const checkpointPath = "/api/conversations/7/thinking/checkpoint"
	for _, tc := range []struct{ path, body string }{
		{chatPath, `{"question":"接着想","mode":"unsupported"}`},
		{chatPath, `{"question":"普通聊天","source":{"session_id":1,"seq_start":1,"seq_end":1}}`},
		{chatPath, `{"question":"普通聊天","mode":"chat","resume_from":1}`},
		{chatPath, `{"question":"继续","mode":"thinking","resume_from":0}`},
		{chatPath, `{"mode":"thinking","resume_from":-1}`},
		{chatPath, `{"question":"  "}`},
		{"/api/chat/0", `{"question":"继续","mode":"thinking"}`},
		{checkpointPath, `{"text":`},
		{checkpointPath, `{"text":"  "}`},
		{checkpointPath, `{"text":"停在这个问题上","direction":"guess"}`},
		{"/api/conversations/0/thinking/checkpoint", `{"text":"我的疑问"}`},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s %s: HTTP %d, want 400: %s", tc.path, tc.body, recorder.Code, recorder.Body)
		}
	}
}
