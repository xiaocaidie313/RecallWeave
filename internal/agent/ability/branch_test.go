package ability

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"recallweave/internal/llm"
	"recallweave/internal/store"
)

func branchToolForTest(t *testing.T, messages []store.ContextMessage) llm.Tool {
	t.Helper()
	tools := NewBranchContextAbility(messages).Tools()
	if len(tools) != 1 || tools[0].Schema.Name != GetMessagesToolName {
		t.Fatalf("branch must expose only bounded source reads, got %+v", tools)
	}
	return tools[0]
}

func readBranchMessages(t *testing.T, tool llm.Tool, sessionID uint, start, end int) getMessagesResult {
	t.Helper()
	arguments := fmt.Sprintf(`{"session_id":%d,"seq_start":%d,"seq_end":%d}`, sessionID, start, end)
	raw, err := tool.Run(context.Background(), arguments)
	if err != nil {
		t.Fatal(err)
	}
	var result getMessagesResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode original messages: %v", err)
	}
	if result.SessionID != sessionID {
		t.Fatalf("source session changed: got %d, want %d", result.SessionID, sessionID)
	}
	return result
}

func TestBranchContextPreservesOriginalAndOwnMessages(t *testing.T) {
	// The inherited prefix need not have the branch's own session or numbering.
	tool := branchToolForTest(t, []store.ContextMessage{
		{SessionID: 10, Seq: 3, Role: "user", Content: "旧方案：先保留 PostgreSQL。"},
		{SessionID: 10, Seq: 8, Role: "assistant", Content: "这是建议，不是用户决定。"},
		{SessionID: 20, Seq: 1, Role: "user", Content: "在支线研究另一种方案。"},
		{SessionID: 20, Seq: 2, Role: "assistant", Content: "支线自己的回答。"},
	})
	inherited := readBranchMessages(t, tool, 10, 1, 10)
	wantInherited := []messageLine{
		{Seq: 3, Role: "user", Content: "旧方案：先保留 PostgreSQL。"},
		{Seq: 8, Role: "assistant", Content: "这是建议，不是用户决定。"},
	}
	if !reflect.DeepEqual(inherited.Messages, wantInherited) {
		t.Fatalf("inherited roles/content/source seq lost: %+v", inherited.Messages)
	}
	own := readBranchMessages(t, tool, 20, 1, 2)
	wantOwn := []messageLine{
		{Seq: 1, Role: "user", Content: "在支线研究另一种方案。"},
		{Seq: 2, Role: "assistant", Content: "支线自己的回答。"},
	}
	if !reflect.DeepEqual(own.Messages, wantOwn) {
		t.Fatalf("branch history changed: %+v", own.Messages)
	}
	limited := readBranchMessages(t, tool, 10, 8, 8)
	if !reflect.DeepEqual(limited.Messages, wantInherited[1:]) {
		t.Fatalf("range included messages outside request: %+v", limited.Messages)
	}
}

func TestBranchContextOwnsImmutableCopy(t *testing.T) {
	input := make([]store.ContextMessage, 1, 2)
	input[0] = store.ContextMessage{SessionID: 10, Seq: 3, Role: "user", Content: "分叉时的原文"}
	tool := branchToolForTest(t, input)
	input[0].Role = "assistant"
	input[0].Content = "父会话后来被修改的内容"
	input = append(input, store.ContextMessage{SessionID: 10, Seq: 4, Role: "user", Content: "分叉之后的父会话消息"})
	got := readBranchMessages(t, tool, 10, 1, 10)
	want := []messageLine{{Seq: 3, Role: "user", Content: "分叉时的原文"}}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Fatalf("caller mutation leaked into branch: %+v", got.Messages)
	}
}

func TestBranchContextRejectsUnknownSessionsAndDoesNotExpandKnownRanges(t *testing.T) {
	tool := branchToolForTest(t, []store.ContextMessage{{SessionID: 10, Seq: 3, Role: "user", Content: "snapshot"}})
	for _, arguments := range []string{
		`{"session_id":999,"seq_start":1,"seq_end":1}`,
		`{"session_id":999,"seq_start":1,"seq_end":50}`,
	} {
		result, err := tool.Run(context.Background(), arguments)
		if err == nil || result != "" {
			t.Fatalf("unknown session was readable: result=%q err=%v", result, err)
		}
	}
	for _, bounds := range [][2]int{{1, 2}, {4, 20}} {
		result := readBranchMessages(t, tool, 10, bounds[0], bounds[1])
		if result.Messages == nil || len(result.Messages) != 0 {
			t.Fatalf("unavailable known range must return an empty list: %+v", result)
		}
	}
	empty := branchToolForTest(t, nil)
	if result, err := empty.Run(context.Background(), `{"session_id":10,"seq_start":1,"seq_end":1}`); err == nil || result != "" {
		t.Fatalf("empty context must not fall back to another source: %q, %v", result, err)
	}
}

func TestBranchContextMessageRangeLimit(t *testing.T) {
	messages := make([]store.ContextMessage, 51)
	for i := range messages {
		messages[i] = store.ContextMessage{SessionID: 10, Seq: i + 1, Role: "user", Content: fmt.Sprint(i + 1)}
	}
	tool := branchToolForTest(t, messages)
	for _, bounds := range [][2]int{{1, 50}, {2, 51}} {
		result := readBranchMessages(t, tool, 10, bounds[0], bounds[1])
		if len(result.Messages) != 50 || result.Messages[0].Seq != bounds[0] || result.Messages[49].Seq != bounds[1] {
			t.Fatalf("valid 50-message range changed: %+v", result.Messages)
		}
	}
	for _, arguments := range []string{
		`{"session_id":10,"seq_start":1,"seq_end":51}`,
		`{"session_id":10,"seq_start":100,"seq_end":150}`,
	} {
		if result, err := tool.Run(context.Background(), arguments); err == nil || result != "" {
			t.Fatalf("51-message request must fail even without matching messages: %q, %v", result, err)
		}
	}
}

func TestBranchContextRejectsInvalidArguments(t *testing.T) {
	tool := branchToolForTest(t, []store.ContextMessage{{SessionID: 10, Seq: 1, Role: "user", Content: "original"}})
	for _, arguments := range []string{
		`not-json`, `null`, `{}`, `[]`,
		`{"session_id":10,"seq_start":1,"seq_end":1} {}`,
		`{"seq_start":1,"seq_end":1}`,
		`{"session_id":10,"seq_end":1}`,
		`{"session_id":10,"seq_start":1}`,
		`{"session_id":0,"seq_start":1,"seq_end":1}`,
		`{"session_id":-1,"seq_start":1,"seq_end":1}`,
		`{"session_id":"10","seq_start":1,"seq_end":1}`,
		`{"session_id":10,"seq_start":0,"seq_end":1}`,
		`{"session_id":10,"seq_start":-1,"seq_end":1}`,
		`{"session_id":10,"seq_start":2,"seq_end":1}`,
		`{"session_id":10,"seq_start":1.5,"seq_end":2}`,
		`{"session_id":10,"seq_start":1,"seq_end":100000000000000000000000000000}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			if result, err := tool.Run(context.Background(), arguments); err == nil || result != "" {
				t.Fatalf("invalid arguments succeeded: %q, %v", result, err)
			}
		})
	}
}
