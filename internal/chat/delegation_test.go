package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/agent/subagent"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"
	"recallweave/internal/watch/useage"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This exercises actual SDK request history against a local fake endpoint.
// No paid model, external MCP or real database is used.
func TestDelegationThroughModelHTTP(t *testing.T) {
	var mu sync.Mutex
	var parentCalls, childCalls, reads int
	var capturedErrors []string
	fakeEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			capturedErrors = append(capturedErrors, err.Error())
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		isParent := false
		for _, tool := range request.Tools {
			isParent = isParent || tool.Function.Name == runtime.DelegateTaskToolName
		}
		message := map[string]any{"role": "assistant"}
		if isParent {
			parentCalls++
			if parentCalls == 1 {
				message["tool_calls"] = []any{map[string]any{"id": "delegate", "type": "function", "function": map[string]any{
					"name":      runtime.DelegateTaskToolName,
					"arguments": `{"task":"核查原文","context":"PUBLIC_CONTEXT","expected_output":"结论和来源"}`,
				}}}
			} else {
				found := false
				for _, item := range request.Messages {
					if item.Role != "tool" {
						continue
					}
					var text string
					if json.Unmarshal(item.Content, &text) != nil {
						continue
					}
					var result runtime.DelegationResult
					if json.Unmarshal([]byte(text), &result) == nil && result.ChildRunID != "" {
						found = result.Status == runtime.RunCompleted && result.Summary == "已核对原文" && len(result.Sources) == 1
						if strings.Contains(text, "FULL_CHILD_TOOL_BODY") {
							capturedErrors = append(capturedErrors, "raw child trace was returned to parent")
						}
					}
				}
				if !found {
					capturedErrors = append(capturedErrors, "parent did not receive child result and actual source")
				}
				message["content"] = "主 Agent 综合后的回答"
			}
		} else {
			childCalls++
			encoded, _ := json.Marshal(request.Messages)
			if strings.Contains(string(encoded), "ROOT_PRIVATE_MARKER") {
				capturedErrors = append(capturedErrors, "parent history leaked to child")
			}
			if !strings.Contains(string(encoded), "PUBLIC_CONTEXT") {
				capturedErrors = append(capturedErrors, "explicit task background missing")
			}
			if childCalls == 1 {
				message["tool_calls"] = []any{map[string]any{"id": "read", "type": "function", "function": map[string]any{
					"name": "get_messages", "arguments": `{"session_id":8,"seq_start":2,"seq_end":4}`,
				}}}
			} else {
				message["content"] = "已核对原文"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "test", "object": "chat.completion", "model": "test-model",
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
		})
	}))
	defer fakeEndpoint.Close()
	client, err := llm.NewClient(llm.Config{BaseURL: fakeEndpoint.URL + "/v1", APIKey: "test-only", Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	read := llm.Tool{Schema: llm.ToolSchema{Name: "get_messages", Parameters: map[string]any{"type": "object"}}, Run: func(context.Context, string) (string, error) {
		reads++
		return `{"session_id":8,"messages":[{"seq":3,"content":"FULL_CHILD_TOOL_BODY"}]}`, nil
	}}
	core := agent.NewAgent(1, "recall", client, "", 3, agent.NewAbility("memory", "", "", agent.ToolBox{read}))
	result, err := runtime.Run(context.Background(), core, runtime.Request{
		Input: "请单独核查", ConversationID: 7,
		Preload:   runtime.Preload{History: []llm.Message{{Role: "user", Content: "ROOT_PRIVATE_MARKER"}}},
		Delegator: subagent.NewManager(subagent.Config{MaxRounds: 3}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capturedErrors) != 0 {
		t.Fatal(capturedErrors)
	}
	if parentCalls != 2 || childCalls != 2 || reads != 1 || len(result.State.Children) != 1 {
		t.Fatalf("unexpected calls: parent=%d child=%d reads=%d", parentCalls, childCalls, reads)
	}
	child := result.State.Children[0]
	if child.State.ParentRunID != result.State.RunID || child.State.RunID == result.State.RunID || child.State.ConversationID != 7 {
		t.Fatalf("invalid child attribution: %+v", child.State)
	}
	answer := answerFromResult(result)
	if answer.Text != "主 Agent 综合后的回答" || answer.Usage.Calls != 4 || answer.Usage.PromptTokens != 40 || answer.Usage.CompletionTokens != 8 {
		t.Fatalf("child usage missing or duplicated: %+v", answer)
	}
	if len(result.State.Calls) != 2 || len(child.State.Calls) != 2 {
		t.Fatal("own Run calls must stay separate")
	}
}

func TestBranchDelegationPreservesScopedMessageReader(t *testing.T) {
	globalCalls := 0
	model := &fakeChatGenerator{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "delegate", Name: runtime.DelegateTaskToolName, Arguments: `{"task":"核查继承原文"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "read", Name: "get_messages", Arguments: `{"session_id":10,"seq_start":1,"seq_end":2}`}}},
		{Content: "只找到分叉前原文"},
		{Content: "主线综合"},
	}}
	globalTools := agent.ToolBox{}
	for _, name := range []string{"search_memories", "get_messages", "get_sessions", "deactivate_memory"} {
		globalTools = append(globalTools, llm.Tool{Schema: llm.ToolSchema{Name: name}, Run: func(context.Context, string) (string, error) { globalCalls++; return "GLOBAL_FUTURE_SECRET", nil }})
	}
	core := agent.NewAgent(1, "recall", model, "", 3, agent.NewAbility("memory", "", "", globalTools))
	scoped := branchCore(core, []store.ContextMessage{{SessionID: 10, Seq: 1, Role: "user", Content: "分叉前原文"}})
	result, err := runtime.Run(context.Background(), scoped, runtime.Request{Input: "调查", Delegator: subagent.NewManager(subagent.Config{})})
	if err != nil {
		t.Fatal(err)
	}
	if globalCalls != 0 || len(result.State.Children) != 1 {
		t.Fatalf("global access restored: calls=%d", globalCalls)
	}
	step := result.State.Children[0].State.Steps[0]
	if !strings.Contains(step.Result, "分叉前原文") || strings.Contains(step.Result, "GLOBAL_FUTURE_SECRET") {
		t.Fatalf("wrong scoped result: %s", step.Result)
	}
}

func TestSaveDelegatedRunsPersistsChildAndParentUsage(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	database, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	child := runtime.RunResult{State: runtime.RunState{RunID: "child", ParentRunID: "parent", ConversationID: 9, Status: runtime.RunFailed, Input: "research", Calls: []useage.ModelCallStats{{Usage: useage.TokenUsage{PromptTokens: 7, CompletionTokens: 3}}}}}
	parent := runtime.RunResult{Content: "调查失败，已有信息如下", State: runtime.RunState{RunID: "parent", ConversationID: 9, Status: runtime.RunCompleted, Input: "question", Children: []runtime.RunResult{child}, Calls: []useage.ModelCallStats{{Usage: useage.TokenUsage{PromptTokens: 11, CompletionTokens: 5}}}}}
	for _, row := range []struct {
		id, parent, status, input, content string
		prompt, completion                 int64
	}{
		{"child", "parent", "failed", "research", "", 7, 3},
		{"parent", "", "completed", "question", parent.Content, 18, 8},
	} {
		mock.ExpectBegin()
		mock.ExpectQuery(`INSERT INTO "agent_runs"`).WithArgs(row.id, row.parent, uint(9), row.status, row.input, row.content, sqlmock.AnyArg(), row.prompt, row.completion, int64(0), 0, sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
		mock.ExpectCommit()
	}
	NewAgent(nil, memory.NewMemoryManger(database, nil), nil).saveRun(context.Background(), parent)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(fmt.Errorf("child and parent Run persistence: %w", err))
	}
}

func TestChildEvidenceAppearsInAnswerCitations(t *testing.T) {
	result := runtime.RunResult{State: runtime.RunState{Children: []runtime.RunResult{{State: runtime.RunState{Steps: []runtime.RunStep{
		{ToolName: "search_memories", Result: `{"memories":[{"memory_id":4,"session_id":8,"seq_start":1,"seq_end":5}]}`},
		{ToolName: "get_messages", Result: `{"session_id":8,"messages":[{"seq":3,"content":"原文"}]}`},
	}}}}}}
	answer := answerFromResult(result)
	if len(answer.Citations) != 1 || answer.Citations[0].MemoryID != 4 || answer.Citations[0].SeqStart != 3 || answer.Citations[0].SeqEnd != 3 {
		t.Fatalf("child evidence not checked: %+v", answer.Citations)
	}
}
