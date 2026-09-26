package ask

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/llm"
)

// fakeResponder 按脚本一轮一轮返回，替代真实模型，避免测试发网络请求。
type fakeResponder struct {
	turns []llm.Turn
	err   error

	calls    int
	lastTool []llm.ToolSchema
}

func (f *fakeResponder) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *fakeResponder) Next(_ context.Context, _ *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
	if f.err != nil {
		return llm.Turn{}, f.err
	}

	f.lastTool = tools
	turn := f.turns[f.calls]
	f.calls++
	return turn, nil
}

// recordingTool 记录自己收到的参数，用来确认分发和 JSON 解析是对的。
func recordingTool(name, response string, gotArguments *string) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{Name: name, Description: name},
		Run: func(_ context.Context, arguments string) (string, error) {
			*gotArguments = arguments
			return response, nil
		},
	}
}

func TestAskReturnsAnswerWithoutTools(t *testing.T) {
	responder := &fakeResponder{turns: []llm.Turn{{Content: "没有相关记录。"}}}
	agent := NewAgent(responder, llm.NewToolHandle(), nil, nil)

	answer, err := agent.Ask(context.Background(), "我上次怎么说的", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if answer.Text != "没有相关记录。" {
		t.Fatalf("unexpected answer: %q", answer.Text)
	}
	if answer.Rounds != 1 {
		t.Fatalf("expected 1 round, got %d", answer.Rounds)
	}
}

func TestAskRunsToolThenAnswers(t *testing.T) {
	hits := searchMemoriesResult{Memories: []memoryHit{{
		MemoryID:  7,
		SessionID: 3,
		SeqStart:  1,
		SeqEnd:    2,
		Title:     "cmd 目录的作用",
	}}}
	encoded, err := json.Marshal(hits)
	if err != nil {
		t.Fatalf("marshal hits: %v", err)
	}

	var gotArguments string
	tools := llm.NewToolHandle()
	tools.Register(recordingTool("search_memories", string(encoded), &gotArguments))

	responder := &fakeResponder{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "search_memories",
			Arguments: `{"keyword":"cmd","limit":3}`,
		}}},
		{Content: "你说过 cmd 是可执行入口。"},
	}}

	agent := NewAgent(responder, tools, nil, nil)

	answer, err := agent.Ask(context.Background(), "cmd 目录是干什么的", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotArguments != `{"keyword":"cmd","limit":3}` {
		t.Fatalf("tool received unexpected arguments: %q", gotArguments)
	}
	if answer.Rounds != 2 {
		t.Fatalf("expected 2 rounds, got %d", answer.Rounds)
	}
	if len(answer.Citations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(answer.Citations))
	}
	if answer.Citations[0].SessionID != 3 || answer.Citations[0].SeqStart != 1 {
		t.Fatalf("unexpected citation: %+v", answer.Citations[0])
	}
	// 工具列表每轮都要发给模型，否则模型第二轮就不知道自己有哪些能力。
	if len(responder.lastTool) != 1 || responder.lastTool[0].Name != "search_memories" {
		t.Fatalf("unexpected tool schemas: %+v", responder.lastTool)
	}
}

func TestAskDeduplicatesCitations(t *testing.T) {
	hits := searchMemoriesResult{Memories: []memoryHit{
		{MemoryID: 4, SessionID: 1, SeqStart: 1, SeqEnd: 2, Title: "cmd 目录"},
	}}
	encoded, err := json.Marshal(hits)
	if err != nil {
		t.Fatalf("marshal hits: %v", err)
	}

	var ignored string
	tools := llm.NewToolHandle()
	tools.Register(recordingTool("search_memories", string(encoded), &ignored))

	// 模型换了关键词又搜一次，两次都命中同一条记忆。
	responder := &fakeResponder{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_memories", Arguments: `{"keyword":"cmd"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "call_2", Name: "search_memories", Arguments: `{"keyword":"目录"}`}}},
		{Content: "你说过 cmd 是可执行入口。"},
	}}

	answer, err := NewAgent(responder, tools, nil, nil).Ask(context.Background(), "问题", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(answer.Citations) != 1 {
		t.Fatalf("expected deduplicated citation, got %d: %+v", len(answer.Citations), answer.Citations)
	}
}

func TestAskFeedsToolErrorBackToModel(t *testing.T) {
	tools := llm.NewToolHandle()
	tools.Register(llm.Tool{
		Schema: llm.ToolSchema{Name: "search_memories"},
		Run: func(context.Context, string) (string, error) {
			return "", errors.New("db is down")
		},
	})

	responder := &fakeResponder{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_memories", Arguments: "{}"}}},
		{Content: "检索失败了，暂时查不到。"},
	}}

	agent := NewAgent(responder, tools, nil, nil)

	answer, err := agent.Ask(context.Background(), "问题", 0)
	if err != nil {
		t.Fatalf("tool failure should not abort the loop: %v", err)
	}
	if answer.Text != "检索失败了，暂时查不到。" {
		t.Fatalf("unexpected answer: %q", answer.Text)
	}
}

func TestAskStopsAfterMaxRounds(t *testing.T) {
	tools := llm.NewToolHandle()
	tools.Register(llm.Tool{
		Schema: llm.ToolSchema{Name: "search_memories"},
		Run: func(context.Context, string) (string, error) {
			return `{"memories":[]}`, nil
		},
	})

	turns := make([]llm.Turn, maxRounds)
	for i := range turns {
		turns[i] = llm.Turn{ToolCalls: []llm.ToolCall{{
			ID:   "call",
			Name: "search_memories",
		}}}
	}

	agent := NewAgent(&fakeResponder{turns: turns}, tools, nil, nil)

	_, err := agent.Ask(context.Background(), "问题", 0)
	if err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("expected give-up error, got %v", err)
	}
}

func TestAskWithoutModel(t *testing.T) {
	agent := NewAgent(nil, llm.NewToolHandle(), nil, nil)

	if _, err := agent.Ask(context.Background(), "问题", 0); !errors.Is(err, ErrNoModel) {
		t.Fatalf("expected ErrNoModel, got %v", err)
	}
}

func TestUnknownToolIsReportedToModel(t *testing.T) {
	responder := &fakeResponder{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "no_such_tool"}}},
		{Content: "换个方式回答。"},
	}}

	agent := NewAgent(responder, llm.NewToolHandle(), nil, nil)

	answer, err := agent.Ask(context.Background(), "问题", 0)
	if err != nil {
		t.Fatalf("unknown tool should not abort the loop: %v", err)
	}
	if answer.Text != "换个方式回答。" {
		t.Fatalf("unexpected answer: %q", answer.Text)
	}
}
