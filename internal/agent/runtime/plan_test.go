package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
	"recallweave/internal/plan"
)

func updatePlanCall(id, arguments string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: plan.ToolName, Arguments: arguments}
}

func TestRunExecutesAndCompletesPlan(t *testing.T) {
	model := &scriptedModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{
			updatePlanCall("plan", `{"goal":"读取并总结","steps":[{"step":"读取资料","status":"in_progress"},{"step":"汇总结果","status":"pending"}]}`),
			{ID: "read", Name: "echo", Arguments: `{}`},
		}},
		{ToolCalls: []llm.ToolCall{updatePlanCall("done", `{"goal":"读取并总结","steps":[{"step":"读取资料","status":"completed","result":"工具返回资料"},{"step":"汇总结果","status":"completed","result":"已整理资料"}]}`)}},
		{Content: "总结完成"},
	}}
	a := agent.NewAgent(1, "test", model, "", 4, echoAbility(func(context.Context, string) (string, error) { return "资料", nil }))
	result, err := Run(context.Background(), a, Request{Input: "读取并总结"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "总结完成" || result.State.Status != RunCompleted || result.State.Plan == nil || result.State.Plan.HasOpenSteps() {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.State.Steps) != 3 || result.State.Steps[1].PlanStep != "读取资料" || result.State.Steps[1].Result != "资料" {
		t.Fatalf("execution must be linked to the active step: %+v", result.State.Steps)
	}
	// Reuse the same Agent: plans must not leak into the next Run.
	model.turns = append(model.turns, llm.Turn{Content: "你好"})
	next, err := Run(context.Background(), a, Request{Input: "你好"})
	if err != nil || next.State.Plan != nil || next.State.CurrentRound != 1 {
		t.Fatalf("plan leaked between runs: %+v, %v", next, err)
	}
}

func TestRunReplansAfterToolFailure(t *testing.T) {
	attempts := 0
	model := &scriptedModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{
			updatePlanCall("plan", `{"goal":"查资料","steps":[{"step":"查询远端","status":"in_progress"},{"step":"总结","status":"pending"}]}`),
			{ID: "remote", Name: "echo"},
		}},
		{ToolCalls: []llm.ToolCall{
			updatePlanCall("revise", `{"goal":"查资料","explanation":"远端不可用，改查本地","steps":[{"step":"查询远端","status":"blocked","result":"远端不可用"},{"step":"查询本地","status":"in_progress"},{"step":"总结","status":"pending"}]}`),
			{ID: "local", Name: "echo"},
		}},
		{ToolCalls: []llm.ToolCall{updatePlanCall("done", `{"goal":"查资料","steps":[{"step":"查询远端","status":"blocked","result":"远端不可用"},{"step":"查询本地","status":"completed","result":"已返回本地资料"},{"step":"总结","status":"completed","result":"已总结本地结果"}]}`)}},
		{Content: "已根据本地资料总结，远端信息暂不可用。"},
	}}
	a := agent.NewAgent(1, "test", model, "", 5, echoAbility(func(context.Context, string) (string, error) {
		attempts++
		if attempts == 1 {
			return "", errors.New("remote unavailable")
		}
		return "本地资料", nil
	}))
	result, err := Run(context.Background(), a, Request{Input: "查资料"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Status != RunPartial || result.State.Plan == nil || len(result.State.Plan.Steps) != 3 {
		t.Fatalf("expected revised partial plan, got %+v", result)
	}
	if len(result.State.Errors) != 1 || result.State.Errors[0].Fatal || result.State.Steps[3].PlanStep != "查询本地" {
		t.Fatalf("failure and revised execution must remain traceable: %+v", result.State)
	}
}

func TestRunRequiresOpenPlanToBeUpdatedBeforeFinishing(t *testing.T) {
	model := &scriptedModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{updatePlanCall("plan", `{"goal":"查资料","steps":[{"step":"查询","status":"pending"}]}`)}},
		{Content: "完成了"},
		{ToolCalls: []llm.ToolCall{updatePlanCall("blocked", `{"goal":"查资料","steps":[{"step":"查询","status":"blocked","result":"没有可用的数据源"}]}`)}},
		{Content: "没有可用数据源，暂时无法完成查询。"},
	}}
	result, err := Run(context.Background(), agent.NewAgent(1, "test", model, "", 4), Request{Input: "查资料"})
	if err != nil || result.State.Status != RunPartial || model.calls != 4 || result.Content == "完成了" {
		t.Fatalf("unfinished plan was silently accepted: %+v, %v", result, err)
	}
}

func TestRunPlanCannotExceedRoundBudget(t *testing.T) {
	model := &scriptedModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{updatePlanCall("plan", `{"goal":"查资料","steps":[{"step":"查询","status":"pending"}]}`)}},
		{Content: "完成了"},
	}}
	result, err := Run(context.Background(), agent.NewAgent(1, "test", model, "", 2), Request{Input: "查资料"})
	if err == nil || !strings.Contains(err.Error(), "cannot finish") || result.State.Status != RunFailed || model.calls != 2 {
		t.Fatalf("expected bounded failure with plan retained: %+v, %v", result, err)
	}
	if result.State.Plan == nil || !result.State.Plan.HasOpenSteps() {
		t.Fatal("failed run must preserve unfinished plan")
	}
}

func TestPlanToolRejectsInvalidUpdatesWithoutChangingPlan(t *testing.T) {
	state := RunState{}
	tool := planTool(&state)
	if _, err := tool.Run(context.Background(), `{"goal":"查询","steps":[{"step":"检索","status":"in_progress"}]}`); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state.Plan)
	for _, arguments := range []string{
		`{"goal":"查询","steps":[{"step":"检索","status":"completed"}]}`,
		`{"goal":"查询","steps":[{"step":"检索","status":"unknown"}]}`,
		`{"goal":"查询","steps":[{"step":"检索","status":"in_progress"},{"step":"核对","status":"in_progress"}]}`,
		`{"goal":"查询","steps":[{"step":"改用其他方案","status":"pending"}]}`,
	} {
		if _, err := tool.Run(context.Background(), arguments); err == nil {
			t.Errorf("expected rejection for %s", arguments)
		}
		after, _ := json.Marshal(state.Plan)
		if string(after) != string(before) {
			t.Fatal("invalid update changed the current plan")
		}
	}
}

func TestRunRejectsPlanningToolCollision(t *testing.T) {
	conflict := agent.NewAbility("conflict", "", "", agent.ToolBox{{Schema: llm.ToolSchema{Name: plan.ToolName}}})
	result, err := Run(context.Background(), agent.NewAgent(1, "test", &scriptedModel{}, "", 2, conflict), Request{Input: "hi"})
	if err == nil || result.State.Status != RunFailed || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("runtime tool was shadowed: %+v, %v", result, err)
	}
}
