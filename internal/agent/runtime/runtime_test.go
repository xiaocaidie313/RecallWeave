package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
)

type scriptedModel struct {
	turns []llm.Turn
	err   error
	calls int
}

func (m *scriptedModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", nil
}

func (m *scriptedModel) Next(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
	if m.err != nil {
		return llm.Turn{}, m.err
	}
	turn := m.turns[m.calls]
	m.calls++
	return turn, nil
}

func echoAbility(run llm.ToolFunc) *agent.Ability {
	return agent.NewAbility("echo", "echo", "", agent.ToolBox{{
		Schema: llm.ToolSchema{Name: "echo"},
		Run:    run,
	}})
}

func TestRunAnswersWithoutTools(t *testing.T) {
	a := agent.NewAgent(7, "test", &scriptedModel{turns: []llm.Turn{{Content: "你好"}}}, "", 3)

	result, err := Run(context.Background(), a, Request{Input: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "你好" || result.State.Status != RunCompleted {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.State.AgentID != 7 || result.State.CurrentRound != 1 || result.State.FinishedAt == nil {
		t.Fatalf("unexpected state: %+v", result.State)
	}
}

func TestRunExecutesToolThenAnswers(t *testing.T) {
	var gotArguments string
	ability := echoAbility(func(_ context.Context, arguments string) (string, error) {
		gotArguments = arguments
		return `{"ok":true}`, nil
	})
	model := &scriptedModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "echo", Arguments: `{"x":1}`}}},
		{Content: "done"},
	}}

	result, err := Run(context.Background(), agent.NewAgent(1, "test", model, "", 3, ability), Request{Input: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotArguments != `{"x":1}` {
		t.Fatalf("tool received %q", gotArguments)
	}
	if len(result.State.Steps) != 1 || result.State.Steps[0].Result != `{"ok":true}` {
		t.Fatalf("unexpected steps: %+v", result.State.Steps)
	}
	if result.State.CurrentRound != 2 {
		t.Fatalf("expected 2 rounds, got %d", result.State.CurrentRound)
	}
}

func TestRunContinuesAfterToolError(t *testing.T) {
	ability := echoAbility(func(context.Context, string) (string, error) {
		return "", errors.New("db is down")
	})
	model := &scriptedModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "echo"}}},
		{Content: "查不到"},
	}}

	result, err := Run(context.Background(), agent.NewAgent(1, "test", model, "", 3, ability), Request{Input: "hi"})
	if err != nil {
		t.Fatalf("tool failure should not abort the run: %v", err)
	}
	if len(result.State.Errors) != 1 || result.State.Errors[0].Fatal {
		t.Fatalf("expected one non-fatal error, got %+v", result.State.Errors)
	}
	if result.State.Steps[0].Error != "db is down" {
		t.Fatalf("unexpected step: %+v", result.State.Steps[0])
	}
}

func TestRunStopsAfterMaxRounds(t *testing.T) {
	ability := echoAbility(func(context.Context, string) (string, error) { return "{}", nil })
	turns := make([]llm.Turn, 2)
	for i := range turns {
		turns[i] = llm.Turn{ToolCalls: []llm.ToolCall{{ID: "call", Name: "echo"}}}
	}

	result, err := Run(context.Background(), agent.NewAgent(1, "test", &scriptedModel{turns: turns}, "", 2, ability), Request{Input: "hi"})
	if err == nil || !strings.Contains(err.Error(), "maximum tool rounds") {
		t.Fatalf("expected max rounds error, got %v", err)
	}
	if result.State.Status != RunFailed {
		t.Fatalf("expected failed status, got %s", result.State.Status)
	}
}

func TestRunRecordsModelCalls(t *testing.T) {
	model := &namedModel{
		scriptedModel: &scriptedModel{turns: []llm.Turn{{
			Content: "你好",
			Usage:   llm.Usage{PromptTokens: 11, CompletionTokens: 3, CachedTokens: 2},
		}}},
		name: "test-model",
	}

	result, err := Run(context.Background(), agent.NewAgent(1, "test", model, "", 3), Request{Input: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.State.Calls) != 1 {
		t.Fatalf("expected 1 model call, got %d", len(result.State.Calls))
	}
	call := result.State.Calls[0]
	if call.Round != 1 || call.Model != "test-model" {
		t.Fatalf("unexpected call: %+v", call)
	}
	if call.Usage.PromptTokens != 11 || call.Usage.CompletionTokens != 3 || call.Usage.CachedTokens != 2 {
		t.Fatalf("unexpected usage: %+v", call.Usage)
	}
	if call.RunID != result.State.RunID {
		t.Fatalf("call run id %q != state %q", call.RunID, result.State.RunID)
	}
}

type namedModel struct {
	*scriptedModel
	name string
}

func (m namedModel) ModelName() string { return m.name }

func TestRunWithoutModel(t *testing.T) {
	if _, err := Run(context.Background(), agent.NewAgent(1, "test", nil, "", 3), Request{Input: "hi"}); err == nil {
		t.Fatal("expected error for nil model")
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := Run(ctx, agent.NewAgent(1, "test", &scriptedModel{}, "", 3), Request{Input: "hi"})
	if !errors.Is(err, context.Canceled) || result.State.Status != RunCancelled {
		t.Fatalf("expected cancelled run, got status %s, err %v", result.State.Status, err)
	}
}
