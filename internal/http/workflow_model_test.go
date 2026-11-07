//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"recallweave/internal/agent/ability"
	"recallweave/internal/llm"
	"recallweave/internal/types"
)

const (
	workflowFirstQuestion  = "项目选择 PostgreSQL，因为需要关系查询和 pgvector 记忆检索。"
	workflowSecondQuestion = "部署先用 Docker Compose。"
	workflowRecallQuestion = "之前为什么选择 PostgreSQL？"
	workflowFirstAnswer    = "已记录：项目选择 PostgreSQL，以支持关系查询和 pgvector 记忆检索。"
	workflowSecondAnswer   = "已记录：先使用 Docker Compose 部署。"
	workflowRecallAnswer   = "之前选择 PostgreSQL，是因为项目需要关系查询和 pgvector 记忆检索。"
)

type workflowToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type workflowMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	ToolCallID string             `json:"tool_call_id"`
	ToolCalls  []workflowToolCall `json:"tool_calls"`
}

type workflowChatRequest struct {
	Messages []workflowMessage `json:"messages"`
}

type workflowModelStats struct {
	ChatCalls      int
	SegmentCalls   int
	EmbeddingCalls int
	SearchCalls    int
	SourceCalls    int
}

type workflowModel struct {
	t                 *testing.T
	mu                sync.Mutex
	requests          []workflowChatRequest
	stats             workflowModelStats
	extractionStarted chan struct{}
	extractionRelease chan struct{}
	releaseOnce       sync.Once
}

func newWorkflowModel(t *testing.T) (*workflowModel, *httptest.Server) {
	t.Helper()
	model := &workflowModel{
		t: t, extractionStarted: make(chan struct{}), extractionRelease: make(chan struct{}),
	}
	server := httptest.NewServer(http.HandlerFunc(model.serveHTTP))
	t.Cleanup(func() {
		model.ReleaseExtraction()
		server.Close()
	})
	return model, server
}

func (m *workflowModel) WaitExtractionStarted(ctx context.Context) error {
	select {
	case <-m.extractionStarted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *workflowModel) ReleaseExtraction() {
	m.releaseOnce.Do(func() { close(m.extractionRelease) })
}

func (m *workflowModel) ChatRequests() []workflowChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := make([]workflowChatRequest, len(m.requests))
	for i, request := range m.requests {
		requests[i].Messages = append([]workflowMessage(nil), request.Messages...)
		for j := range requests[i].Messages {
			requests[i].Messages[j].ToolCalls = append([]workflowToolCall(nil), request.Messages[j].ToolCalls...)
		}
	}
	return requests
}

func (m *workflowModel) Stats() workflowModelStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

func (m *workflowModel) AssertHistory(t *testing.T) {
	t.Helper()
	for _, request := range m.ChatRequests() {
		if !strings.Contains(workflowLastUser(request.Messages), "部署先用 Docker Compose") {
			continue
		}
		question, answer := -1, -1
		for i, message := range request.Messages {
			if message.Role == "user" && message.Content == workflowFirstQuestion {
				question = i
			}
			if message.Role == "assistant" && message.Content == workflowFirstAnswer {
				answer = i
			}
		}
		if question >= 0 && answer > question && answer < len(request.Messages)-1 {
			return
		}
		t.Fatalf("second chat request lost the first user/assistant turn: %+v", request.Messages)
	}
	t.Fatal("model did not receive the second chat request")
}

func (m *workflowModel) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/embeddings":
		m.mu.Lock()
		m.stats.EmbeddingCalls++
		m.mu.Unlock()
		vector := make([]float64, types.EmbeddingDim)
		vector[0] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": "workflow-embedding",
			"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": vector}},
			"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
		})
	case "/v1/chat/completions":
		var request workflowChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			m.fail(w, "invalid chat request: %v", err)
			return
		}
		prompt := request.Messages[0].Content
		switch {
		case strings.Contains(prompt, "记忆提炼助手"):
			m.segment(w, r, request)
		case strings.Contains(prompt, "简短标题"):
			m.complete(w, "PostgreSQL 项目决策", nil)
		case strings.Contains(prompt, "把对话压缩"):
			m.complete(w, "项目选择 PostgreSQL，并先使用 Docker Compose 部署。", nil)
		default:
			m.chat(w, request)
		}
	default:
		m.fail(w, "unexpected model path: %s", r.URL.Path)
	}
}

var workflowNumberedMessage = regexp.MustCompile(`(?m)^([0-9]+): \(([^)]+)\) (.*)$`)

func (m *workflowModel) segment(w http.ResponseWriter, r *http.Request, request workflowChatRequest) {
	m.mu.Lock()
	m.stats.SegmentCalls++
	first := m.stats.SegmentCalls == 1
	m.mu.Unlock()
	if first {
		close(m.extractionStarted)
		select {
		case <-m.extractionRelease:
		case <-r.Context().Done():
			return
		}
	}
	lines := workflowNumberedMessage.FindAllStringSubmatch(workflowLastUser(request.Messages), -1)
	if len(lines) == 0 {
		m.fail(w, "extraction request has no numbered messages")
		return
	}
	segments := make([]llm.Segment, 0, (len(lines)+1)/2)
	for i := 0; i < len(lines); i += 2 {
		end := i + 1
		if end >= len(lines) {
			end = i
		}
		startSeq, _ := strconv.Atoi(lines[i][1])
		endSeq, _ := strconv.Atoi(lines[end][1])
		content := lines[i][3]
		title, contentType := "会话记录", "other"
		if strings.Contains(content, "项目选择 PostgreSQL") {
			title, contentType = "PostgreSQL 选型", "decision"
		} else if strings.Contains(content, "部署先用 Docker Compose") {
			title, contentType = "Docker Compose 部署", "decision"
		}
		segments = append(segments, llm.Segment{
			SeqStart: startSeq, SeqEnd: endSeq, Title: title, Summary: content, Tag: "tech", ContentType: contentType,
		})
	}
	encoded, err := json.Marshal(map[string]any{"segments": segments})
	if err != nil {
		m.fail(w, "encode segments: %v", err)
		return
	}
	m.complete(w, string(encoded), nil)
}

func (m *workflowModel) chat(w http.ResponseWriter, request workflowChatRequest) {
	m.mu.Lock()
	m.stats.ChatCalls++
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	question := workflowLastUser(request.Messages)
	switch {
	case strings.Contains(question, "之前为什么选择 PostgreSQL"):
		m.recall(w, request)
	case strings.Contains(question, "部署先用 Docker Compose"):
		m.complete(w, workflowSecondAnswer, nil)
	case strings.Contains(question, "项目选择 PostgreSQL"):
		m.complete(w, workflowFirstAnswer, nil)
	default:
		m.fail(w, "unexpected workflow question: %q", question)
	}
}

func (m *workflowModel) recall(w http.ResponseWriter, request workflowChatRequest) {
	results := workflowToolResults(request.Messages)
	if source, ok := results[ability.GetMessagesToolName]; ok {
		var original struct {
			SessionID uint `json:"session_id"`
			Messages  []struct {
				Seq     int    `json:"seq"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(source), &original) != nil || original.SessionID == 0 {
			m.fail(w, "invalid get_messages result: %s", source)
			return
		}
		foundQuestion, foundAnswer := false, false
		for _, message := range original.Messages {
			foundQuestion = foundQuestion || message.Content == workflowFirstQuestion
			foundAnswer = foundAnswer || message.Content == workflowFirstAnswer
		}
		if !foundQuestion || !foundAnswer {
			m.fail(w, "source read did not return original PostgreSQL turn: %s", source)
			return
		}
		m.complete(w, workflowRecallAnswer, nil)
		return
	}
	if raw, ok := results[ability.SearchMemoriesToolName]; ok {
		var search ability.SearchMemoriesResult
		if json.Unmarshal([]byte(raw), &search) != nil {
			m.fail(w, "invalid search_memories result: %s", raw)
			return
		}
		for _, memory := range search.Memories {
			if memory.SessionID == 0 || !strings.Contains(memory.Title+memory.Content, "PostgreSQL") {
				continue
			}
			m.mu.Lock()
			m.stats.SourceCalls++
			m.mu.Unlock()
			m.complete(w, "", workflowCall("workflow-source", ability.GetMessagesToolName, map[string]any{
				"session_id": memory.SessionID, "seq_start": memory.SeqStart, "seq_end": memory.SeqEnd,
			}))
			return
		}
		m.fail(w, "search did not return a PostgreSQL memory: %s", raw)
		return
	}
	m.mu.Lock()
	m.stats.SearchCalls++
	m.mu.Unlock()
	m.complete(w, "", workflowCall("workflow-search", ability.SearchMemoriesToolName, map[string]any{
		"keyword": "PostgreSQL", "limit": 5,
	}))
}

func workflowLastUser(messages []workflowMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

func workflowToolResults(messages []workflowMessage) map[string]string {
	names, results := make(map[string]string), make(map[string]string)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			names[call.ID] = call.Function.Name
		}
		if message.Role == "tool" {
			if name := names[message.ToolCallID]; name != "" {
				results[name] = message.Content
			}
		}
	}
	return results
}

func workflowCall(id, name string, arguments map[string]any) []workflowToolCall {
	encoded, _ := json.Marshal(arguments)
	call := workflowToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, string(encoded)
	return []workflowToolCall{call}
}

func (m *workflowModel) complete(w http.ResponseWriter, content string, calls []workflowToolCall) {
	message := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if len(calls) > 0 {
		message["tool_calls"], finish = calls, "tool_calls"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "workflow-completion", "object": "chat.completion", "created": 1, "model": "workflow-model",
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
		"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18},
	})
}

func (m *workflowModel) fail(w http.ResponseWriter, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	m.t.Error(message)
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "workflow_test_error"}})
}
