package ask

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/llm"
)

type fakeChatGenerator struct {
	turns []llm.Turn
	err   error

	calls    int
	lastTool []llm.ToolSchema
}

func (f *fakeChatGenerator) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *fakeChatGenerator) Next(_ context.Context, _ *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
	if f.err != nil {
		return llm.Turn{}, f.err
	}

	f.lastTool = tools
	turn := f.turns[f.calls]
	f.calls++
	return turn, nil
}

func recordingTool(name, response string, gotArguments *string) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{Name: name, Description: name},
		Run: func(_ context.Context, arguments string) (string, error) {
			*gotArguments = arguments
			return response, nil
		},
	}
}

func newAskService(model llm.ChatGenerator, abilities ...*agent.Ability) *Agent {
	return NewAgent(agent.NewAgent(1, "recall", model, "test", 3, abilities...), nil, nil)
}

func TestAskReturnsAnswerWithoutTools(t *testing.T) {
	model := &fakeChatGenerator{turns: []llm.Turn{{Content: "没有相关记录。"}}}
	service := newAskService(model)

	answer, err := service.Ask(context.Background(), "我上次怎么说的", 0)
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
	hits := ability.SearchMemoriesResult{Memories: []ability.MemoryHit{{
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
	memory := agent.NewAbility("memory", "检索记忆", "", agent.ToolBox{
		recordingTool(ability.SearchMemoriesToolName, string(encoded), &gotArguments),
	})
	model := &fakeChatGenerator{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      ability.SearchMemoriesToolName,
			Arguments: `{"keyword":"cmd","limit":3}`,
		}}},
		{Content: "你说过 cmd 是可执行入口。"},
	}}

	answer, err := newAskService(model, memory).Ask(context.Background(), "cmd 目录是干什么的", 0)
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
	if len(model.lastTool) != 1 || model.lastTool[0].Name != ability.SearchMemoriesToolName {
		t.Fatalf("unexpected tool schemas: %+v", model.lastTool)
	}
}

func TestAskWithoutModel(t *testing.T) {
	if _, err := newAskService(nil).Ask(context.Background(), "问题", 0); !errors.Is(err, ErrNoModel) {
		t.Fatalf("expected ErrNoModel, got %v", err)
	}
}
