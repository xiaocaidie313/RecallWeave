package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
)

const testBackgroundTaskID = "fedcba9876543210fedcba9876543210"

type backgroundTestDelegator struct {
	continuationTestDelegator
	backgroundEnabled bool
	submit            func(context.Context, DelegationParent, DelegationRequest) (BackgroundTask, error)
	submitFollowup    func(context.Context, DelegationParent, FollowupRequest) (BackgroundTask, error)
	get               func(context.Context, DelegationParent, string) (BackgroundTask, error)
	cancel            func(context.Context, DelegationParent, string) (BackgroundTask, error)
}

func (d backgroundTestDelegator) BackgroundEnabled() bool { return d.backgroundEnabled }
func (d backgroundTestDelegator) AvailableAgents() []DelegationAgent {
	return []DelegationAgent{{Name: "researcher", Description: "Find sources"}, {Name: "reviewer", Description: "Check claims"}}
}
func (d backgroundTestDelegator) DefaultAgent() string { return "researcher" }
func (d backgroundTestDelegator) Submit(ctx context.Context, parent DelegationParent, request DelegationRequest) (BackgroundTask, error) {
	if d.submit == nil {
		return BackgroundTask{}, errors.New("unexpected background submit")
	}
	return d.submit(ctx, parent, request)
}
func (d backgroundTestDelegator) SubmitFollowUp(ctx context.Context, parent DelegationParent, request FollowupRequest) (BackgroundTask, error) {
	if d.submitFollowup == nil {
		return BackgroundTask{}, errors.New("unexpected background follow-up")
	}
	return d.submitFollowup(ctx, parent, request)
}
func (d backgroundTestDelegator) GetTask(ctx context.Context, parent DelegationParent, id string) (BackgroundTask, error) {
	if d.get == nil {
		return BackgroundTask{}, errors.New("unexpected background get")
	}
	return d.get(ctx, parent, id)
}
func (d backgroundTestDelegator) CancelTask(ctx context.Context, parent DelegationParent, id string) (BackgroundTask, error) {
	if d.cancel == nil {
		return BackgroundTask{}, errors.New("unexpected background cancel")
	}
	return d.cancel(ctx, parent, id)
}

func backgroundReceipt(parent DelegationParent) BackgroundTask {
	return BackgroundTask{TaskID: testBackgroundTaskID, SubagentID: testSubagentID,
		ParentRunID: parent.RunID, ConversationID: parent.ConversationID, Agent: "researcher", Task: "research", Status: "queued"}
}

func TestBackgroundSubmissionReturnsHandleWithoutAttachingChildOrUsage(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{
		delegationCall(`{"agent":"reviewer","task":"check supplied sources","background":true}`),
		{Content: "Started in background; we can continue talking."},
	}}
	core := agent.NewAgent(1, "main", model, "", 2)
	starts := 0
	d := backgroundTestDelegator{backgroundEnabled: true, submit: func(_ context.Context, parent DelegationParent, request DelegationRequest) (BackgroundTask, error) {
		starts++
		if parent.Agent != core || parent.RunID == "" || parent.ConversationID != 19 || request.Agent != "reviewer" || !request.Background {
			t.Fatalf("scope or selection changed: %+v %+v", parent, request)
		}
		return backgroundReceipt(parent), nil
	}}
	result, err := Run(context.Background(), core, Request{Input: "investigate in background", ConversationID: 19, Delegator: d})
	if err != nil {
		t.Fatal(err)
	}
	var receipt BackgroundTask
	if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &receipt); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || receipt.TaskID != testBackgroundTaskID || receipt.Status != "queued" || receipt.Result != nil || len(result.State.Children) != 0 || len(result.State.AllCalls()) != 2 {
		t.Fatalf("submission was treated as completed work: starts=%d receipt=%+v state=%+v", starts, receipt, result.State)
	}
}

func TestBackgroundToolsOnlyAppearOnEnabledRootRun(t *testing.T) {
	for _, tc := range []struct {
		name, parent string
		enabled      bool
	}{
		{name: "root", enabled: true}, {name: "disabled"}, {name: "child", enabled: true, parent: "parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &delegationScriptModel{turns: []llm.Turn{{Content: "direct"}}}
			_, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "hi", ParentRunID: tc.parent, Delegator: backgroundTestDelegator{backgroundEnabled: tc.enabled}})
			if err != nil {
				t.Fatal(err)
			}
			want := tc.enabled && tc.parent == ""
			for _, name := range []string{GetSubagentTaskToolName, CancelSubagentTaskToolName} {
				found := false
				for _, schema := range model.schemas[0] {
					found = found || schema.Name == name
				}
				if found != want {
					t.Fatalf("%s visible=%v want=%v", name, found, want)
				}
			}
		})
	}
}

func TestBackgroundQueryAndCancellationPreserveScopeWithoutRebillingHistory(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{
		continuationCall(GetSubagentTaskToolName, `{"task_id":"`+testBackgroundTaskID+`"}`),
		continuationCall(CancelSubagentTaskToolName, `{"task_id":"`+testBackgroundTaskID+`"}`),
		{Content: "Task result retrieved."},
	}}
	core := agent.NewAgent(1, "main", model, "", 3)
	gets, cancels := 0, 0
	action := func(parent DelegationParent, id string) BackgroundTask {
		if parent.Agent != core || parent.ConversationID != 31 || id != testBackgroundTaskID {
			t.Fatalf("task action lost scope: %+v %s", parent, id)
		}
		task := backgroundReceipt(parent)
		task.Status = "completed"
		child := delegationChild(parent, RunCompleted)
		summary := SummarizeDelegation(child, nil)
		task.Result = &summary
		return task
	}
	d := backgroundTestDelegator{backgroundEnabled: true,
		get: func(_ context.Context, parent DelegationParent, id string) (BackgroundTask, error) {
			gets++
			return action(parent, id), nil
		},
		cancel: func(_ context.Context, parent DelegationParent, id string) (BackgroundTask, error) {
			cancels++
			return action(parent, id), nil
		},
	}
	result, err := Run(context.Background(), core, Request{Input: "check and stop task", ConversationID: 31, Delegator: d})
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 || cancels != 1 || len(result.State.Children) != 0 || len(result.State.AllCalls()) != 3 {
		t.Fatalf("old work was billed again: %+v", result.State)
	}
	var reply BackgroundTask
	if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &reply); err != nil || reply.Result == nil || reply.Result.Usage.PromptTokens != 13 {
		t.Fatalf("historical usage missing from read-only result: %+v %v", reply, err)
	}
}

func TestBackgroundAndSynchronousDelegationShareBudget(t *testing.T) {
	for _, firstBackground := range []bool{true, false} {
		first, second := `{"task":"research","background":true}`, `{"task":"research"}`
		if !firstBackground {
			first, second = second, first
		}
		turn := delegationCall(first)
		turn.ToolCalls = append(turn.ToolCalls, delegationCall(second).ToolCalls...)
		model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
		starts := 0
		d := backgroundTestDelegator{backgroundEnabled: true,
			continuationTestDelegator: continuationTestDelegator{runtimeTestDelegator: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
				starts++
				return delegationChild(parent, RunCompleted), nil
			}},
			submit: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (BackgroundTask, error) {
				starts++
				return backgroundReceipt(parent), nil
			},
		}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", Delegator: d})
		if err != nil || starts != 1 || !strings.Contains(result.State.Steps[1].Error, "one delegation") {
			t.Fatalf("background=%v bypassed shared budget: starts=%d result=%+v err=%v", firstBackground, starts, result, err)
		}
	}
}

func TestBackgroundRejectedSubmissionCanBeCorrected(t *testing.T) {
	turn := delegationCall(`{"task":"research","background":true}`)
	turn.ToolCalls = append(turn.ToolCalls, turn.ToolCalls...)
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
	starts := 0
	d := backgroundTestDelegator{backgroundEnabled: true, submit: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (BackgroundTask, error) {
		starts++
		if starts == 1 {
			return BackgroundTask{}, errors.New("queue full")
		}
		return backgroundReceipt(parent), nil
	}}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", Delegator: d})
	if err != nil || starts != 2 || result.State.Steps[0].Error != "queue full" || result.State.Steps[1].Error != "" || len(result.State.Children) != 0 {
		t.Fatalf("rejected start consumed quota: starts=%d result=%+v err=%v", starts, result, err)
	}
}

func TestBackgroundFollowupSharesSynchronousFollowupBudget(t *testing.T) {
	for _, firstBackground := range []bool{true, false} {
		first := `{"subagent_id":"` + testSubagentID + `","message":"check","background":true}`
		second := `{"subagent_id":"` + testSubagentID + `","message":"check"}`
		if !firstBackground {
			first, second = second, first
		}
		turn := continuationCall(FollowupTaskToolName, first)
		turn.ToolCalls = append(turn.ToolCalls, continuationCall(FollowupTaskToolName, second).ToolCalls...)
		model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
		starts := 0
		d := backgroundTestDelegator{backgroundEnabled: true,
			continuationTestDelegator: continuationTestDelegator{enabled: true, follow: func(_ context.Context, parent DelegationParent, _ FollowupRequest) (RunResult, error) {
				starts++
				return delegationChild(parent, RunCompleted), nil
			}},
			submitFollowup: func(_ context.Context, parent DelegationParent, request FollowupRequest) (BackgroundTask, error) {
				starts++
				if !request.Background || request.SubagentID != testSubagentID {
					t.Fatalf("follow-up changed: %+v", request)
				}
				return backgroundReceipt(parent), nil
			},
		}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "continue", Delegator: d})
		if err != nil || starts != 1 || !strings.Contains(result.State.Steps[1].Error, "one follow-up") {
			t.Fatalf("background=%v bypassed follow-up budget: %+v %v", firstBackground, result, err)
		}
	}
}

func TestBackgroundTaskActionsRejectInvalidArgumentsBeforeManager(t *testing.T) {
	for _, name := range []string{GetSubagentTaskToolName, CancelSubagentTaskToolName} {
		for _, arguments := range []string{"", `null`, `[]`, `{}`, `{"task_id":"short"}`, `{"task_id":"` + strings.ToUpper(testBackgroundTaskID) + `"}`, `{"task_id":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`, `{"task_id":"` + testBackgroundTaskID + `","conversation_id":2}`, `{"task_id":"` + testBackgroundTaskID + `"}{}`, `{"task_id":3}`} {
			model := &delegationScriptModel{turns: []llm.Turn{continuationCall(name, arguments), {Content: "invalid"}}}
			d := backgroundTestDelegator{backgroundEnabled: true}
			result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "check", Delegator: d})
			if err != nil || result.State.Steps[0].Error == "" || strings.Contains(result.State.Steps[0].Error, "unexpected background") {
				t.Fatalf("invalid args reached manager: %s %s %+v %v", name, arguments, result, err)
			}
		}
	}
}

func TestUnavailableBackgroundSubmissionDoesNotFallBackToSynchronous(t *testing.T) {
	for _, name := range []string{DelegateTaskToolName, FollowupTaskToolName} {
		arguments := `{"task":"research","background":true}`
		if name == FollowupTaskToolName {
			arguments = `{"subagent_id":"` + testSubagentID + `","message":"check","background":true}`
		}
		model := &delegationScriptModel{turns: []llm.Turn{continuationCall(name, arguments), {Content: "unavailable"}}}
		d := backgroundTestDelegator{continuationTestDelegator: continuationTestDelegator{enabled: true}}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", Delegator: d})
		if err != nil || !strings.Contains(result.State.Steps[0].Error, "background execution is unavailable") {
			t.Fatalf("unavailable background executed synchronously: %+v %v", result, err)
		}
	}
}

func TestBackgroundToolNamesAreReserved(t *testing.T) {
	for _, name := range []string{GetSubagentTaskToolName, CancelSubagentTaskToolName} {
		model := &delegationScriptModel{}
		core := agent.NewAgent(1, "main", model, "", 2, agent.NewAbility("spoof", "", "", agent.ToolBox{{Schema: llm.ToolSchema{Name: name}}}))
		result, err := Run(context.Background(), core, Request{Input: "hi"})
		if err == nil || !strings.Contains(err.Error(), "reserved") || model.calls != 0 || result.State.Status != RunFailed {
			t.Fatalf("reserved tool was accepted: %s %+v %v", name, result, err)
		}
	}
}
