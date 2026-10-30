package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/agent/subagent"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

type chatContinuationStore struct {
	mu   sync.Mutex
	rows map[string]store.SubagentSession
}

func cloneChatContinuation(row store.SubagentSession) store.SubagentSession {
	row.Snapshot = append(json.RawMessage(nil), row.Snapshot...)
	row.Preset = append(json.RawMessage(nil), row.Preset...)
	row.Sources = append(json.RawMessage(nil), row.Sources...)
	return row
}

func (s *chatContinuationStore) Create(ctx context.Context, row *store.SubagentSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = make(map[string]store.SubagentSession)
	}
	if _, exists := s.rows[row.ID]; exists {
		return errors.New("duplicate child")
	}
	row.Revision = 1
	row.CreatedAt = time.Now()
	row.UpdatedAt = row.CreatedAt
	s.rows[row.ID] = cloneChatContinuation(*row)
	return nil
}

func (s *chatContinuationStore) Load(ctx context.Context, id string, conversationID uint) (store.SubagentSession, error) {
	if err := ctx.Err(); err != nil {
		return store.SubagentSession{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.ConversationID != conversationID {
		return store.SubagentSession{}, errors.New("child unavailable")
	}
	return cloneChatContinuation(row), nil
}

func (s *chatContinuationStore) Update(ctx context.Context, row *store.SubagentSession, expected uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[row.ID]
	if !ok || old.ConversationID != row.ConversationID || old.Revision != expected {
		return subagent.ErrSessionConflict
	}
	row.Revision = expected + 1
	row.CreatedAt = old.CreatedAt
	row.UpdatedAt = time.Now()
	s.rows[row.ID] = cloneChatContinuation(*row)
	return nil
}

func (s *chatContinuationStore) List(ctx context.Context, conversationID uint, limit int) ([]store.SubagentSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]store.SubagentSession, 0)
	for _, row := range s.rows {
		if row.ConversationID == conversationID {
			rows = append(rows, cloneChatContinuation(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// Real SDK requests are sent to a local fake endpoint. Manager reconstruction
// shares only durable data, so this verifies continuation does not depend on
// keeping an Agent object or Conversation pointer alive between parent Runs.
func TestSubagentContinuationThroughModelHTTPAcrossParentRuns(t *testing.T) {
	const originalArguments = `{"session_id":8,"seq_start":3,"seq_end":3}`
	const originalResult = `{"session_id":8,"messages":[{"seq":3,"content":"RAW_TOOL_BODY: 最初使用 Go"}]}`
	var mu sync.Mutex
	phase := 1
	parentCalls := make(map[int]int)
	childCalls := make(map[int]int)
	var capturedErrors []string
	var firstReply, secondReply runtime.DelegationResult
	var listedID string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var request struct {
			Messages []struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"tool_call_id"`
				ToolCalls  []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			capturedErrors = append(capturedErrors, err.Error())
			http.Error(w, "bad test request", http.StatusBadRequest)
			return
		}
		tools := make(map[string]bool)
		for _, tool := range request.Tools {
			tools[tool.Function.Name] = true
		}
		isParent := tools[runtime.DelegateTaskToolName]
		message := map[string]any{"role": "assistant"}
		call := func(id, name, arguments string) {
			message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}}
		}
		if isParent {
			parentCalls[phase]++
			if !tools[runtime.FollowupTaskToolName] || !tools[runtime.ListSubagentsToolName] {
				capturedErrors = append(capturedErrors, "parent continuation tools missing")
			}
			for _, item := range request.Messages {
				if item.Role != "tool" {
					continue
				}
				var text string
				if json.Unmarshal(item.Content, &text) != nil {
					continue
				}
				if phase == 1 {
					var reply runtime.DelegationResult
					if json.Unmarshal([]byte(text), &reply) == nil && reply.ChildRunID != "" {
						firstReply = reply
					}
				} else {
					var list struct {
						Subagents []runtime.SubagentInfo `json:"subagents"`
					}
					if json.Unmarshal([]byte(text), &list) == nil && len(list.Subagents) > 0 {
						listedID = list.Subagents[0].SubagentID
					}
					var reply runtime.DelegationResult
					if json.Unmarshal([]byte(text), &reply) == nil && reply.ChildRunID != "" {
						secondReply = reply
					}
				}
				if strings.Contains(text, "RAW_TOOL_BODY") {
					capturedErrors = append(capturedErrors, "parent received raw child transcript instead of compact result")
				}
			}
			if phase == 1 && parentCalls[phase] == 1 {
				call("delegate_task_1", runtime.DelegateTaskToolName, `{"agent":"researcher","task":"核查最初语言","context":"PUBLIC_CONTEXT","expected_output":"原文与结论"}`)
			} else if phase == 1 {
				message["content"] = "主 Agent 第一轮综合"
			} else {
				switch parentCalls[phase] {
				case 1:
					call("list_1", runtime.ListSubagentsToolName, `{}`)
				case 2:
					arguments, _ := json.Marshal(runtime.FollowupRequest{SubagentID: listedID, Message: "再核对已有材料，保留之前结论"})
					call("followup_1", runtime.FollowupTaskToolName, string(arguments))
				default:
					message["content"] = "主 Agent 第二轮综合"
				}
			}
		} else {
			childCalls[phase]++
			if tools[runtime.FollowupTaskToolName] || tools[runtime.ListSubagentsToolName] {
				capturedErrors = append(capturedErrors, "child received parent orchestration tools")
			}
			encoded, _ := json.Marshal(request.Messages)
			if strings.Contains(string(encoded), "ROOT_PRIVATE_MARKER") {
				capturedErrors = append(capturedErrors, "parent private history leaked to child")
			}
			if !strings.Contains(string(encoded), "PUBLIC_CONTEXT") {
				capturedErrors = append(capturedErrors, "child explicit background missing")
			}
			if phase == 1 {
				if childCalls[phase] == 1 {
					call("original_read", "get_messages", originalArguments)
				} else {
					message["content"] = "FIRST_CHILD_FINAL: 已核对原文"
				}
			} else {
				var callFound, resultFound, finalFound, followupFound bool
				for _, item := range request.Messages {
					for _, toolCall := range item.ToolCalls {
						if toolCall.ID == "original_read" && toolCall.Function.Name == "get_messages" && toolCall.Function.Arguments == originalArguments {
							callFound = true
						}
					}
					var text string
					_ = json.Unmarshal(item.Content, &text)
					if item.Role == "tool" && item.ToolCallID == "original_read" && text == originalResult {
						resultFound = true
					}
					if item.Role == "assistant" && text == "FIRST_CHILD_FINAL: 已核对原文" {
						finalFound = true
					}
					if item.Role == "user" && strings.Contains(text, "再核对已有材料") {
						followupFound = true
					}
				}
				if !callFound || !resultFound || !finalFound || !followupFound {
					capturedErrors = append(capturedErrors, fmt.Sprintf("restored history incomplete: call=%v result=%v final=%v followup=%v", callFound, resultFound, finalFound, followupFound))
				}
				message["content"] = "SECOND_CHILD_FINAL: 沿用实际原文完成补查"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "test", "object": "chat.completion", "model": "test-model",
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
		})
	}))
	defer endpoint.Close()
	client, err := llm.NewClient(llm.Config{BaseURL: endpoint.URL + "/v1", APIKey: "test-only", Model: "test-model", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	reader := llm.Tool{Schema: llm.ToolSchema{Name: "get_messages", Parameters: map[string]any{"type": "object"}}, Run: func(_ context.Context, arguments string) (string, error) {
		reads++
		if arguments != originalArguments {
			return "", fmt.Errorf("unexpected read arguments: %s", arguments)
		}
		return originalResult, nil
	}}
	core := agent.NewAgent(1, "main", client, "", 5, agent.NewAbility("memory", "", "", agent.ToolBox{reader}))
	repository := &chatContinuationStore{}
	newManager := func() *subagent.Manager {
		manager, err := subagent.NewManager(subagent.Config{})
		if err != nil {
			t.Fatal(err)
		}
		return manager.WithSessions(repository)
	}
	first, err := runtime.Run(context.Background(), core, runtime.Request{Input: "第一次调查", ConversationID: 17, Delegator: newManager(), Preload: runtime.Preload{History: []llm.Message{{Role: "user", Content: "ROOT_PRIVATE_MARKER first parent"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.State.Children) != 1 {
		t.Fatalf("initial child missing: %+v", first.State)
	}
	firstChild := first.State.Children[0]
	if firstReply.SubagentID == "" || firstReply.SubagentID != firstChild.State.SubagentID || !firstReply.CanFollowUp {
		t.Fatalf("initial child not continuable: %+v", firstReply)
	}

	mu.Lock()
	phase = 2
	mu.Unlock()
	// No child Agent, Conversation pointer or Manager is retained here. Only
	// the persisted repository is reused for the next parent request.
	second, err := runtime.Run(context.Background(), core, runtime.Request{Input: "继续刚才的调查", ConversationID: 17, Delegator: newManager(), Preload: runtime.Preload{History: []llm.Message{{Role: "user", Content: "ROOT_PRIVATE_MARKER second parent"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(capturedErrors) != 0 {
		t.Fatal(capturedErrors)
	}
	if len(second.State.Children) != 1 {
		t.Fatalf("continued child missing: %+v", second.State)
	}
	secondChild := second.State.Children[0]
	if listedID != firstChild.State.SubagentID || secondChild.State.SubagentID != firstChild.State.SubagentID || secondChild.State.RunID == firstChild.State.RunID || secondChild.State.PreviousRunID != firstChild.State.RunID || secondChild.State.ParentRunID != second.State.RunID {
		t.Fatalf("child identity chain lost: first=%+v second=%+v", firstChild.State, secondChild.State)
	}
	if reads != 1 || parentCalls[1] != 2 || childCalls[1] != 2 || parentCalls[2] != 3 || childCalls[2] != 1 || len(secondChild.State.Steps) != 0 {
		t.Fatalf("tools replayed or unexpected model loop: reads=%d parent=%v child=%v", reads, parentCalls, childCalls)
	}
	if secondReply.SubagentID != firstReply.SubagentID || !secondReply.CanFollowUp || secondReply.Agent != "researcher" || secondReply.Summary != "SECOND_CHILD_FINAL: 沿用实际原文完成补查" {
		t.Fatalf("bad follow-up reply: %+v", secondReply)
	}
	if len(secondReply.Sources) != 1 || secondReply.Sources[0].SourceRunID != firstChild.State.RunID || secondReply.Sources[0].SessionID != 8 || secondReply.Sources[0].SeqStart != 3 || secondReply.Sources[0].SeqEnd != 3 {
		t.Fatalf("inherited evidence provenance lost: %+v", secondReply.Sources)
	}
	answer := answerFromResult(second)
	if answer.Text != "主 Agent 第二轮综合" || answer.Usage.Calls != 4 || answer.Usage.PromptTokens != 40 || answer.Usage.CompletionTokens != 8 || len(second.State.Calls) != 3 || len(secondChild.State.Calls) != 1 {
		t.Fatalf("old model usage replayed into new Run: %+v", answer)
	}
	row, err := repository.Load(context.Background(), firstReply.SubagentID, 17)
	if err != nil || row.Revision != 2 || row.LatestRunID != secondChild.State.RunID {
		t.Fatalf("continued context not persisted: row=%+v err=%v", row, err)
	}
}
