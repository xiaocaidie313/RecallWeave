package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSegmentRequestsAndDecodesContentType(t *testing.T) {
	var prompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			t.Errorf("invalid request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt = request.Messages[0].Content
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"role":    "assistant",
				"content": `{"segments":[{"seq_start":1,"seq_end":1,"title":"偏好","summary":"用户喜欢简短回答","tag":"other","content_type":"preference"}]}`,
			}}},
		})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-key", Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	segments, err := client.Segment(context.Background(), []Message{{Seq: 1, Role: "user", Content: "我喜欢简短回答"}}, []string{"other"}, []string{"preference", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "preference, other") || !strings.Contains(prompt, "content_type") {
		t.Fatalf("content type contract missing from prompt: %s", prompt)
	}
	if len(segments) != 1 || segments[0].ContentType != "preference" || segments[0].SeqStart != 1 {
		t.Fatalf("content type or provenance lost: %+v", segments)
	}
}
