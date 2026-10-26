package ability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

type fakeMemoryBackend struct {
	items         []store.Memory
	deactivations int
	err           error
}

func (f *fakeMemoryBackend) SearchMemories(_ context.Context, _ string, _ store.MemoryTag, _ int, includeInactive ...bool) ([]store.Memory, error) {
	items := []store.Memory{}
	for _, item := range f.items {
		if item.Status == store.MemoryActive || (len(includeInactive) > 0 && includeInactive[0]) {
			items = append(items, item)
		}
	}
	return items, nil
}
func (f *fakeMemoryBackend) GetMessages(context.Context, uint, int, int) ([]store.Message, error) {
	return []store.Message{{Seq: 1, Role: "user", Content: "旧方案用 Python"}}, nil
}
func (f *fakeMemoryBackend) GetSessions(context.Context, uint) ([]store.Session, error) {
	return nil, nil
}
func (f *fakeMemoryBackend) DeactivateMemory(_ context.Context, id uint, reason string) (store.Memory, error) {
	f.deactivations++
	if f.err != nil {
		return store.Memory{}, f.err
	}
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].Status = store.MemoryInactive
			f.items[i].DeactivationReason = reason
			return f.items[i], nil
		}
	}
	return store.Memory{}, errors.New("not found")
}

func TestDeactivateToolAndHistorySearch(t *testing.T) {
	backend := &fakeMemoryBackend{items: []store.Memory{
		{ID: 1, SessionID: 10, SeqStart: 1, SeqEnd: 2, Content: "旧方案用 Python", Status: store.MemoryActive},
		{ID: 2, SessionID: 20, SeqStart: 1, SeqEnd: 2, Content: "现方案用 Go", Status: store.MemoryActive},
	}}
	tools := llm.NewToolHandle()
	for _, tool := range NewMemoryAbility(backend).Tools() {
		tools.Register(tool)
	}
	result, err := tools.Run(context.Background(), DeactivateMemoryToolName, `{"memory_id":1,"reason":"用户明确要求不再使用 Python 方案"}`)
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct {
		MemoryID uint               `json:"memory_id"`
		Status   store.MemoryStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(result), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.MemoryID != 1 || outcome.Status != store.MemoryInactive || backend.deactivations != 1 {
		t.Fatalf("unexpected tool result: %s", result)
	}
	for _, tc := range []struct {
		arguments string
		count     int
	}{
		{`{"keyword":"项目方案"}`, 1},
		{`{"keyword":"项目方案","include_inactive":true}`, 2},
	} {
		result, err := tools.Run(context.Background(), SearchMemoriesToolName, tc.arguments)
		if err != nil {
			t.Fatal(err)
		}
		var found SearchMemoriesResult
		if err := json.Unmarshal([]byte(result), &found); err != nil {
			t.Fatal(err)
		}
		if len(found.Memories) != tc.count {
			t.Fatalf("unexpected visibility: %s", result)
		}
	}
	if backend.items[0].Content != "旧方案用 Python" || backend.items[1].Status != store.MemoryActive {
		t.Fatalf("unrelated memory or original changed: %+v", backend.items)
	}
	result, err = tools.Run(context.Background(), GetMessagesToolName, `{"session_id":10,"seq_start":1,"seq_end":2}`)
	if err != nil || result == "" {
		t.Fatalf("original must remain readable: %s %v", result, err)
	}
}

func TestDeactivateToolRejectsBadArgumentsAndReportsFailure(t *testing.T) {
	backend := &fakeMemoryBackend{err: errors.New("write failed")}
	tool := deactivateMemoryTool(backend)
	for _, arguments := range []string{`{}`, `{"memory_id":1}`, `{"memory_id":-1,"reason":"原因"}`, `{"memory_id":1,"reason":" "}`} {
		if _, err := tool.Run(context.Background(), arguments); err == nil {
			t.Fatalf("invalid arguments accepted: %s", arguments)
		}
	}
	if backend.deactivations != 0 {
		t.Fatal("bad arguments reached storage")
	}
	if result, err := tool.Run(context.Background(), `{"memory_id":1,"reason":"用户要求"}`); err == nil || result != "" {
		t.Fatalf("must not confirm a failed write: %s %v", result, err)
	}
}

type correctionModel struct {
	turns []llm.Turn
	calls int
}

func (*correctionModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", nil
}
func (m *correctionModel) Next(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
	turn := m.turns[m.calls]
	m.calls++
	return turn, nil
}

func TestAgentDeactivationPersistsAcrossRuns(t *testing.T) {
	backend := &fakeMemoryBackend{items: []store.Memory{
		{ID: 1, Content: "Python 旧方案", Status: store.MemoryActive},
		{ID: 2, Content: "Go 当前方案", Status: store.MemoryActive},
	}}
	model := &correctionModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "locate", Name: SearchMemoriesToolName, Arguments: `{"keyword":"项目方案"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "disable", Name: DeactivateMemoryToolName, Arguments: `{"memory_id":1,"reason":"用户明确要求停用 Python 旧方案"}`}}},
		{Content: "已停用 Python 旧方案记忆，原文保留。"},
		{ToolCalls: []llm.ToolCall{{ID: "current", Name: SearchMemoriesToolName, Arguments: `{"keyword":"项目方案"}`}}},
		{Content: "目前使用 Go。"},
	}}
	a := agent.NewAgent(1, "test", model, "", 4, NewMemoryAbility(backend))
	first, err := runtime.Run(context.Background(), a, runtime.Request{Input: "请停用 Python 的旧方案记忆", ConversationID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.State.Steps) != 2 || first.State.Steps[1].ToolName != DeactivateMemoryToolName || !strings.Contains(first.State.Steps[1].Result, "inactive") {
		t.Fatalf("deactivation was not recorded in Run: %+v", first.State.Steps)
	}
	second, err := runtime.Run(context.Background(), a, runtime.Request{Input: "目前项目用什么语言？", ConversationID: 2})
	if err != nil {
		t.Fatal(err)
	}
	var hits SearchMemoriesResult
	if len(second.State.Steps) != 1 {
		t.Fatalf("unexpected steps: %+v", second.State.Steps)
	}
	if err := json.Unmarshal([]byte(second.State.Steps[0].Result), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits.Memories) != 1 || hits.Memories[0].MemoryID != 2 || second.Content != "目前使用 Go。" {
		t.Fatalf("inactive memory leaked into a new run: %+v", second)
	}
}
