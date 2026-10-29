package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
	"recallweave/internal/watch/useage"
)

type runtimeTestDelegator func(context.Context, DelegationParent, DelegationRequest) (RunResult, error)

func (f runtimeTestDelegator) Delegate(ctx context.Context, parent DelegationParent, request DelegationRequest) (RunResult, error) {
	return f(ctx, parent, request)
}

type delegationScriptModel struct {
	turns   []llm.Turn
	calls   int
	schemas [][]llm.ToolSchema
}

func (m *delegationScriptModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected one-turn call")
}

func (m *delegationScriptModel) Next(_ context.Context, _ *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
	m.schemas = append(m.schemas, append([]llm.ToolSchema(nil), tools...))
	if m.calls >= len(m.turns) {
		return llm.Turn{}, fmt.Errorf("unexpected model call %d", m.calls+1)
	}
	turn := m.turns[m.calls]
	m.calls++
	return turn, nil
}

func delegationCall(arguments string) llm.Turn {
	return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "delegate_1", Name: DelegateTaskToolName, Arguments: arguments}}}
}

func delegationChild(parent DelegationParent, status RunStatus) RunResult {
	return RunResult{
		Content: "调查结论",
		State: RunState{
			RunID: "child_" + parent.RunID, ParentRunID: parent.RunID,
			ConversationID: parent.ConversationID, AgentID: 2, AgentName: "researcher", Status: status,
			Calls: []useage.ModelCallStats{{
				RunID: "child_" + parent.RunID, Round: 1, LatencyMs: 17,
				Usage: useage.TokenUsage{PromptTokens: 13, CompletionTokens: 7, ReasoningTokens: 2, CachedTokens: 3},
			}},
		},
	}
}

func delegationSchemaPresent(schemas []llm.ToolSchema) bool {
	for _, schema := range schemas {
		if schema.Name == DelegateTaskToolName {
			return true
		}
	}
	return false
}

func TestDelegationToolAvailableOnlyOnConfiguredParentRun(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
		parentID   string
		wantTool   bool
	}{
		{name: "unconfigured"},
		{name: "parent", configured: true, wantTool: true},
		{name: "child", configured: true, parentID: "parent-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &delegationScriptModel{turns: []llm.Turn{{Content: "直接回答"}}}
			core := agent.NewAgent(1, "recall", model, "", 2)
			request := Request{Input: "你好", ParentRunID: tc.parentID}
			if tc.configured {
				request.Delegator = runtimeTestDelegator(func(context.Context, DelegationParent, DelegationRequest) (RunResult, error) {
					t.Fatal("a direct answer must not create a child")
					return RunResult{}, nil
				})
			}
			result, err := Run(context.Background(), core, request)
			if err != nil {
				t.Fatal(err)
			}
			if got := delegationSchemaPresent(model.schemas[0]); got != tc.wantTool {
				t.Fatalf("delegate_task visible=%v, want %v", got, tc.wantTool)
			}
			if len(result.State.Children) != 0 || result.State.ParentRunID != tc.parentID {
				t.Fatalf("unexpected run: %+v", result.State)
			}
		})
	}
}

func TestDelegationRoundTripKeepsTraceAndReturnsCompactResult(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{
		delegationCall(`{"task":"核查原文","context":"必要背景","expected_output":"结论和来源"}`),
		{Content: "根据调查继续回答"},
	}}
	core := agent.NewAgent(1, "recall", model, "", 3)
	delegations := 0
	delegator := runtimeTestDelegator(func(_ context.Context, parent DelegationParent, request DelegationRequest) (RunResult, error) {
		delegations++
		if parent.Agent != core || parent.ConversationID != 23 || parent.RunID == "" {
			t.Fatalf("parent scope was replaced: %+v", parent)
		}
		if request != (DelegationRequest{Task: "核查原文", Context: "必要背景", ExpectedOutput: "结论和来源"}) {
			t.Fatalf("unexpected task payload: %+v", request)
		}
		child := delegationChild(parent, RunCompleted)
		child.State.Steps = []RunStep{{
			ToolName: "get_messages", Arguments: `{"session_id":7,"seq_start":1,"seq_end":50}`,
			Result: `{"session_id":7,"messages":[{"seq":4,"content":"CHILD_RAW_TRACE_MARKER"}]}`,
		}}
		return child, nil
	})
	result, err := Run(context.Background(), core, Request{
		Input: "调查后回复", ConversationID: 23, Delegator: delegator,
		Preload: Preload{Background: []string{"PARENT_PRIVATE_BACKGROUND"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "根据调查继续回答" || model.calls != 2 || delegations != 1 || result.State.Status != RunCompleted {
		t.Fatalf("the parent did not continue after delegation: %+v", result)
	}
	if len(result.State.Children) != 1 || len(result.State.Steps) != 1 {
		t.Fatalf("child trace missing: %+v", result.State)
	}
	child := result.State.Children[0]
	if child.State.ParentRunID != result.State.RunID || child.State.RunID == result.State.RunID || child.State.ConversationID != 23 {
		t.Fatalf("incorrect parent-child linkage: %+v", child.State)
	}
	if !strings.Contains(child.State.Steps[0].Result, "CHILD_RAW_TRACE_MARKER") {
		t.Fatal("full child trace was discarded")
	}
	output := result.State.Steps[0].Result
	for _, private := range []string{"CHILD_RAW_TRACE_MARKER", "PARENT_PRIVATE_BACKGROUND", `"steps"`, `"state"`, `"arguments"`} {
		if strings.Contains(output, private) {
			t.Fatalf("compact result leaked %q: %s", private, output)
		}
	}
	var reply DelegationResult
	if err := json.Unmarshal([]byte(output), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ChildRunID != child.State.RunID || reply.Status != RunCompleted || reply.Summary != "调查结论" {
		t.Fatalf("unexpected result: %+v", reply)
	}
	if reply.Calls != 1 || reply.LatencyMs != 17 || reply.Usage.PromptTokens != 13 || reply.Usage.CompletionTokens != 7 || reply.Usage.CachedTokens != 3 || reply.Usage.ReasoningTokens != 2 {
		t.Fatalf("child usage was lost: %+v", reply)
	}
	if len(reply.Sources) != 1 || reply.Sources[0].SessionID != 7 || reply.Sources[0].SeqStart != 4 || reply.Sources[0].SeqEnd != 4 {
		t.Fatalf("retrieved evidence did not survive the delegation boundary: %+v", reply.Sources)
	}
	if len(result.State.Calls) != 2 || len(result.State.AllCalls()) != 3 {
		t.Fatalf("usage was duplicated or mixed into parent calls: %+v", result.State.Calls)
	}
}

func TestDelegationLimitIsOnePerRunAndResetsForNextRun(t *testing.T) {
	firstTurn := delegationCall(`{"task":"调查"}`)
	firstTurn.ToolCalls = append(firstTurn.ToolCalls, llm.ToolCall{ID: "delegate_2", Name: DelegateTaskToolName, Arguments: `{"task":"再次调查"}`})
	model := &delegationScriptModel{turns: []llm.Turn{firstTurn, {Content: "第一次完成"}, delegationCall(`{"task":"新请求"}`), {Content: "第二次完成"}}}
	core := agent.NewAgent(1, "recall", model, "", 3)
	starts := 0
	delegator := runtimeTestDelegator(func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
		starts++
		return delegationChild(parent, RunCompleted), nil
	})
	first, err := Run(context.Background(), core, Request{Input: "第一次", Delegator: delegator})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || len(first.State.Children) != 1 || len(first.State.Steps) != 2 || first.State.Steps[1].Error == "" {
		t.Fatalf("repeated delegation started another child: starts=%d state=%+v", starts, first.State)
	}
	second, err := Run(context.Background(), core, Request{Input: "第二次", Delegator: delegator})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 2 || len(second.State.Children) != 1 || first.State.RunID == second.State.RunID || len(second.State.Errors) != 0 {
		t.Fatalf("delegation budget leaked across Runs: starts=%d state=%+v", starts, second.State)
	}
}

func TestDelegationInvalidArgumentsNeverStartChild(t *testing.T) {
	encode := func(request DelegationRequest) string {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tc := range []struct{ name, arguments string }{
		{"empty", ""},
		{"malformed", "{"},
		{"null", "null"},
		{"array", "[]"},
		{"missing_task", `{}`},
		{"blank_task", `{"task":"  \n  "}`},
		{"wrong_type", `{"task":123}`},
		{"unknown_policy", `{"task":"调查","max_rounds":100}`},
		{"trailing_object", `{"task":"调查"}{}`},
		{"trailing_junk", `{"task":"调查"} garbage`},
		{"invalid_utf8", "{\"task\":\"" + string([]byte{0xff}) + "\"}"},
		{"task_limit", encode(DelegationRequest{Task: strings.Repeat("x", (8<<10)+1)})},
		{"context_limit", encode(DelegationRequest{Task: "调查", Context: strings.Repeat("x", (16<<10)+1)})},
		{"expected_output_limit", encode(DelegationRequest{Task: "调查", ExpectedOutput: strings.Repeat("x", (2<<10)+1)})},
		{"body_limit", strings.Repeat(" ", 33<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts := 0
			delegator := runtimeTestDelegator(func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
				starts++
				return delegationChild(parent, RunCompleted), nil
			})
			model := &delegationScriptModel{turns: []llm.Turn{delegationCall(tc.arguments), {Content: "参数错误已处理"}}}
			result, err := Run(context.Background(), agent.NewAgent(1, "recall", model, "", 2), Request{Input: "调查", Delegator: delegator})
			if err != nil {
				t.Fatal(err)
			}
			if starts != 0 || len(result.State.Children) != 0 || len(result.State.Steps) != 1 || result.State.Steps[0].Error == "" {
				t.Fatalf("invalid arguments reached manager: starts=%d state=%+v", starts, result.State)
			}
		})
	}
}

func TestDelegationInvalidAttemptDoesNotConsumeValidStart(t *testing.T) {
	turn := delegationCall(`{"task":"调查","max_rounds":100}`)
	turn.ToolCalls = append(turn.ToolCalls, llm.ToolCall{ID: "valid_call", Name: DelegateTaskToolName, Arguments: `{"task":"调查"}`})
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "完成"}}}
	starts := 0
	result, err := Run(context.Background(), agent.NewAgent(1, "recall", model, "", 2), Request{
		Input: "调查", Delegator: runtimeTestDelegator(func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
			starts++
			return delegationChild(parent, RunCompleted), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || len(result.State.Children) != 1 || result.State.Steps[0].Error == "" || result.State.Steps[1].Error != "" {
		t.Fatalf("valid retry was rejected: starts=%d state=%+v", starts, result.State)
	}
}

func TestDelegationChildFailurePreservesRunAndUsage(t *testing.T) {
	for _, status := range []RunStatus{RunFailed, RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			model := &delegationScriptModel{turns: []llm.Turn{delegationCall(`{"task":"调查"}`), {Content: "调查未完成，我会说明限制"}}}
			result, err := Run(context.Background(), agent.NewAgent(1, "recall", model, "", 2), Request{
				Input: "调查", Delegator: runtimeTestDelegator(func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
					child := delegationChild(parent, status)
					child.Content = ""
					child.State.Errors = []RunError{{Round: 1, Message: "child failure", Fatal: true}}
					return child, errors.New("child failure")
				}),
			})
			if err != nil || result.State.Status != RunCompleted || model.calls != 2 {
				t.Fatalf("child failure aborted parent: result=%+v err=%v", result, err)
			}
			if len(result.State.Children) != 1 || result.State.Children[0].State.Status != status || len(result.State.Children[0].State.Errors) != 1 {
				t.Fatalf("failed child trace lost: %+v", result.State)
			}
			var reply DelegationResult
			if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Status != status || reply.Error != "child failure" || reply.Usage.PromptTokens != 13 || reply.Calls != 1 || reply.ChildRunID == "" {
				t.Fatalf("failure response lost state or usage: %+v", reply)
			}
		})
	}
}

func TestDelegationStartupFailureIsRecoverableToolError(t *testing.T) {
	for _, runErr := range []error{errors.New("startup failed"), nil} {
		model := &delegationScriptModel{turns: []llm.Turn{delegationCall(`{"task":"调查"}`), {Content: "无法启动调查"}}}
		result, err := Run(context.Background(), agent.NewAgent(1, "recall", model, "", 2), Request{
			Input: "调查", Delegator: runtimeTestDelegator(func(context.Context, DelegationParent, DelegationRequest) (RunResult, error) {
				return RunResult{}, runErr
			}),
		})
		if err != nil || result.State.Status != RunCompleted || len(result.State.Children) != 0 || len(result.State.Errors) != 1 || result.State.Errors[0].Fatal {
			t.Fatalf("startup failure was not a recoverable tool error: %+v, %v", result, err)
		}
	}
}

func TestDelegationCannotRecurseFromChildEvenWithDelegator(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{delegationCall(`{"task":"再委派"}`), {Content: "自行完成"}}}
	starts := 0
	result, err := Run(context.Background(), agent.NewAgent(2, "researcher", model, "", 2), Request{
		Input: "调查", ParentRunID: "parent-run", Delegator: runtimeTestDelegator(func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
			starts++
			return delegationChild(parent, RunCompleted), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 0 || delegationSchemaPresent(model.schemas[0]) || len(result.State.Children) != 0 || !strings.Contains(result.State.Steps[0].Error, "unknown tool") {
		t.Fatalf("child escaped delegation depth: starts=%d state=%+v", starts, result.State)
	}
}

func TestDelegationReservedNameCannotBeOverridden(t *testing.T) {
	for _, configured := range []bool{false, true} {
		model := &delegationScriptModel{turns: []llm.Turn{{Content: "unexpected"}}}
		toolCalls := 0
		ability := agent.NewAbility("spoof", "", "", agent.ToolBox{{
			Schema: llm.ToolSchema{Name: DelegateTaskToolName},
			Run: func(context.Context, string) (string, error) {
				toolCalls++
				return "unexpected", nil
			},
		}})
		request := Request{Input: "hi"}
		if configured {
			request.Delegator = runtimeTestDelegator(func(context.Context, DelegationParent, DelegationRequest) (RunResult, error) {
				t.Fatal("reserved-name collision must fail before delegation")
				return RunResult{}, nil
			})
		}
		result, err := Run(context.Background(), agent.NewAgent(1, "recall", model, "", 2, ability), request)
		if err == nil || !strings.Contains(err.Error(), "reserved") || result.State.Status != RunFailed || model.calls != 0 || toolCalls != 0 {
			t.Fatalf("reserved tool shadowed runtime: configured=%v result=%+v err=%v", configured, result, err)
		}
	}
}

func TestDelegationParentCancellationPreventsRemainingToolCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := delegationCall(`{"task":"调查"}`)
	turn.ToolCalls = append(turn.ToolCalls, llm.ToolCall{ID: "after_cancel", Name: "echo", Arguments: `{}`})
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "must not run"}}}
	otherCalls := 0
	core := agent.NewAgent(1, "recall", model, "", 2, echoAbility(func(context.Context, string) (string, error) {
		otherCalls++
		return "must not run", nil
	}))
	result, err := Run(ctx, core, Request{
		Input: "调查", Delegator: runtimeTestDelegator(func(childCtx context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
			cancel()
			if !errors.Is(childCtx.Err(), context.Canceled) {
				t.Fatal("parent cancellation was not passed to the manager")
			}
			return delegationChild(parent, RunCancelled), context.Canceled
		}),
	})
	if !errors.Is(err, context.Canceled) || result.State.Status != RunCancelled || model.calls != 1 || otherCalls != 0 {
		t.Fatalf("work continued after cancellation: result=%+v err=%v otherCalls=%d", result, err, otherCalls)
	}
	if len(result.State.Children) != 1 || len(result.State.Steps) != 1 {
		t.Fatalf("canceled child execution was discarded: %+v", result.State)
	}
}
