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

const testGroupID = "1234567890abcdef1234567890abcdef"
const validGroupArgs = `{"goal":"Compare history with sources","context":"Do not browse without authorization","tasks":[{"task":"Read original messages"},{"agent":"reviewer","task":"Check supplied claims"}]}`

type groupTestDelegator struct {
	backgroundTestDelegator
	enabled     bool
	groupSubmit func(context.Context, DelegationParent, GroupDelegationRequest) (TaskGroup, error)
	groupGet    func(context.Context, DelegationParent, string) (TaskGroup, error)
	groupCancel func(context.Context, DelegationParent, string) (TaskGroup, error)
}

func (d groupTestDelegator) GroupsEnabled() bool { return d.enabled }
func (d groupTestDelegator) SubmitGroup(ctx context.Context, parent DelegationParent, request GroupDelegationRequest) (TaskGroup, error) {
	if d.groupSubmit == nil {
		return TaskGroup{}, errors.New("unexpected group submit")
	}
	return d.groupSubmit(ctx, parent, request)
}
func (d groupTestDelegator) GetGroup(ctx context.Context, parent DelegationParent, id string) (TaskGroup, error) {
	if d.groupGet == nil {
		return TaskGroup{}, errors.New("unexpected group get")
	}
	return d.groupGet(ctx, parent, id)
}
func (d groupTestDelegator) CancelGroup(ctx context.Context, parent DelegationParent, id string) (TaskGroup, error) {
	if d.groupCancel == nil {
		return TaskGroup{}, errors.New("unexpected group cancel")
	}
	return d.groupCancel(ctx, parent, id)
}
func groupReceipt(parent DelegationParent) TaskGroup {
	return TaskGroup{GroupID: testGroupID, ConversationID: parent.ConversationID, ParentRunID: parent.RunID, Goal: "Compare history with sources", TaskIDs: []string{testBackgroundTaskID, testSubagentID}, Status: "running"}
}
func groupCall(arguments string) llm.Turn {
	return continuationCall(DelegateTaskGroupToolName, arguments)
}

func TestGroupSubmissionUsesRolesAndReturnsHandleWithoutChildCalls(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{groupCall(validGroupArgs), {Content: "Started both tasks"}}}
	core := agent.NewAgent(1, "main", model, "", 2)
	starts := 0
	d := groupTestDelegator{enabled: true, groupSubmit: func(_ context.Context, parent DelegationParent, request GroupDelegationRequest) (TaskGroup, error) {
		starts++
		if parent.Agent != core || parent.ConversationID != 7 || parent.RunID == "" || len(request.Tasks) != 2 || request.Tasks[0].Agent != "researcher" || request.Tasks[1].Agent != "reviewer" || request.Context != "Do not browse without authorization" {
			t.Fatalf("scope/roles lost: %+v %+v", parent, request)
		}
		return groupReceipt(parent), nil
	}}
	result, err := Run(context.Background(), core, Request{Input: "compare", ConversationID: 7, Delegator: d})
	if err != nil {
		t.Fatal(err)
	}
	var group TaskGroup
	if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &group); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || group.GroupID != testGroupID || len(group.TaskIDs) != 2 || group.Status != "running" || len(result.State.Children) != 0 || len(result.State.AllCalls()) != 2 {
		t.Fatalf("submission became completed/billed work: %+v %+v", group, result.State)
	}
	for _, schema := range model.schemas[0] {
		if schema.Name == DelegateTaskGroupToolName {
			raw, _ := json.Marshal(schema)
			for _, fragment := range []string{"researcher", "reviewer", "Find sources", "Check claims", "\"minItems\":2", "\"maxItems\":2"} {
				if !strings.Contains(string(raw), fragment) {
					t.Fatalf("routing/schema missing %s: %s", fragment, raw)
				}
			}
		}
	}
}

func TestGroupToolsOnlyAppearOnEnabledRoot(t *testing.T) {
	for _, tc := range []struct {
		name, parent string
		enabled      bool
	}{{name: "root", enabled: true}, {name: "disabled"}, {name: "child", parent: "parent", enabled: true}} {
		t.Run(tc.name, func(t *testing.T) {
			model := &delegationScriptModel{turns: []llm.Turn{{Content: "direct"}}}
			_, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "hi", ParentRunID: tc.parent, Delegator: groupTestDelegator{enabled: tc.enabled}})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{DelegateTaskGroupToolName, GetTaskGroupToolName, CancelTaskGroupToolName} {
				found := false
				for _, schema := range model.schemas[0] {
					found = found || schema.Name == name
				}
				if found != (tc.enabled && tc.parent == "") {
					t.Fatalf("%s visibility=%v", name, found)
				}
			}
		})
	}
}

func TestGroupAndSingleShareOneDelegationAction(t *testing.T) {
	for _, groupFirst := range []bool{true, false} {
		first, second := groupCall(validGroupArgs), delegationCall(`{"task":"single","background":true}`)
		if !groupFirst {
			first, second = second, first
		}
		first.ToolCalls = append(first.ToolCalls, second.ToolCalls...)
		model := &delegationScriptModel{turns: []llm.Turn{first, {Content: "done"}}}
		starts := 0
		d := groupTestDelegator{enabled: true, backgroundTestDelegator: backgroundTestDelegator{backgroundEnabled: true, submit: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (BackgroundTask, error) {
			starts++
			return backgroundReceipt(parent), nil
		}}, groupSubmit: func(_ context.Context, parent DelegationParent, _ GroupDelegationRequest) (TaskGroup, error) {
			starts++
			return groupReceipt(parent), nil
		}}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", ConversationID: 9, Delegator: d})
		if err != nil || starts != 1 || !strings.Contains(result.State.Steps[1].Error, "one delegation") {
			t.Fatalf("groupFirst=%v bypassed budget: starts=%d %+v %v", groupFirst, starts, result.State, err)
		}
	}
}

func TestRejectedGroupCanBeCorrectedButAcceptedErrorConsumesBudget(t *testing.T) {
	for _, acceptedError := range []bool{false, true} {
		turn := groupCall(validGroupArgs)
		turn.ToolCalls = append(turn.ToolCalls, turn.ToolCalls...)
		model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
		starts := 0
		d := groupTestDelegator{enabled: true, groupSubmit: func(_ context.Context, parent DelegationParent, _ GroupDelegationRequest) (TaskGroup, error) {
			starts++
			if starts == 1 {
				if acceptedError {
					return groupReceipt(parent), errors.New("accepted with warning")
				}
				return TaskGroup{}, errors.New("capacity full")
			}
			return groupReceipt(parent), nil
		}}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", ConversationID: 8, Delegator: d})
		if err != nil {
			t.Fatal(err)
		}
		if acceptedError {
			if starts != 1 || !strings.Contains(result.State.Steps[0].Result, "accepted with warning") || result.State.Steps[1].Error == "" {
				t.Fatalf("accepted handle lost budget: %+v", result.State)
			}
		} else if starts != 2 || result.State.Steps[0].Error != "capacity full" || result.State.Steps[1].Error != "" {
			t.Fatalf("rejection consumed budget: %+v", result.State)
		}
	}
}

func TestGroupSubmissionRejectsUnknownRolesAndInvalidShape(t *testing.T) {
	invalid := []string{`null`, `{"goal":"x","tasks":[]}`, `{"goal":"x","tasks":[{"task":"a"}]}`, `{"goal":"x","tasks":[{"task":"a"},{"task":"b"},{"task":"c"}]}`, `{"goal":" ","tasks":[{"task":"a"},{"task":"b"}]}`, `{"goal":"x","tasks":[{"task":"a","agent":"unknown"},{"task":"b"}]}`, `{"goal":"x","tasks":[{"task":"a","background":true},{"task":"b"}]}`, `{"goal":"x","conversation_id":99,"tasks":[{"task":"a"},{"task":"b"}]}`, validGroupArgs + ` {}`, `{"goal":"x","tasks":[{"task":"a"},{"task":""}]}`}
	for _, arguments := range invalid {
		model := &delegationScriptModel{turns: []llm.Turn{groupCall(arguments), {Content: "done"}}}
		starts := 0
		d := groupTestDelegator{enabled: true, groupSubmit: func(context.Context, DelegationParent, GroupDelegationRequest) (TaskGroup, error) {
			starts++
			return TaskGroup{}, nil
		}}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", Delegator: d})
		if err != nil || starts != 0 || result.State.Steps[0].Error == "" {
			t.Fatalf("invalid group reached manager: %s %+v %v", arguments, result.State, err)
		}
	}
}

func TestGroupReadAndCancelKeepConversationScopeAndDoNotRebill(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{continuationCall(GetTaskGroupToolName, `{"group_id":"`+testGroupID+`"}`), continuationCall(CancelTaskGroupToolName, `{"group_id":"`+testGroupID+`"}`), {Content: "done"}}}
	core := agent.NewAgent(1, "main", model, "", 3)
	gets, cancels := 0, 0
	action := func(parent DelegationParent, id string) TaskGroup {
		if parent.Agent != core || parent.ConversationID != 29 || id != testGroupID {
			t.Fatalf("scope changed: %+v %s", parent, id)
		}
		group := groupReceipt(parent)
		group.Status = "partial"
		summary := SummarizeDelegation(delegationChild(parent, RunCompleted), nil)
		group.Tasks = []BackgroundTask{{TaskID: testBackgroundTaskID, Result: &summary}}
		return group
	}
	d := groupTestDelegator{enabled: true, groupGet: func(_ context.Context, parent DelegationParent, id string) (TaskGroup, error) {
		gets++
		return action(parent, id), nil
	}, groupCancel: func(_ context.Context, parent DelegationParent, id string) (TaskGroup, error) {
		cancels++
		return action(parent, id), nil
	}}
	result, err := Run(context.Background(), core, Request{Input: "inspect group", ConversationID: 29, Delegator: d})
	if err != nil || gets != 1 || cancels != 1 || len(result.State.Children) != 0 || len(result.State.AllCalls()) != 3 {
		t.Fatalf("scope/history billing incorrect: %+v %v", result.State, err)
	}
}

func TestGroupActionsRejectMalformedIDsBeforeManager(t *testing.T) {
	for _, name := range []string{GetTaskGroupToolName, CancelTaskGroupToolName} {
		for _, arguments := range []string{`{}`, `{"group_id":"short"}`, `{"group_id":"` + strings.ToUpper(testGroupID) + `"}`, `{"group_id":"` + testGroupID + `","conversation_id":99}`, `{"group_id":"` + strings.Repeat("g", 32) + `"}`} {
			model := &delegationScriptModel{turns: []llm.Turn{continuationCall(name, arguments), {Content: "done"}}}
			result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "inspect", Delegator: groupTestDelegator{enabled: true}})
			if err != nil || result.State.Steps[0].Error == "" || strings.Contains(result.State.Steps[0].Error, "unexpected group") {
				t.Fatalf("invalid action reached manager: %s %+v %v", arguments, result.State, err)
			}
		}
	}
}

func TestGroupToolNamesAreReserved(t *testing.T) {
	for _, name := range []string{DelegateTaskGroupToolName, GetTaskGroupToolName, CancelTaskGroupToolName} {
		model := &delegationScriptModel{}
		core := agent.NewAgent(1, "main", model, "", 2, agent.NewAbility("spoof", "", "", agent.ToolBox{{Schema: llm.ToolSchema{Name: name}}}))
		result, err := Run(context.Background(), core, Request{Input: "hi"})
		if err == nil || !strings.Contains(err.Error(), "reserved") || model.calls != 0 || result.State.Status != RunFailed {
			t.Fatalf("reserved tool accepted: %s %+v %v", name, result, err)
		}
	}
}

func TestGroupDelegationKeepsExistingFollowupBudgetIndependent(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{
		groupCall(validGroupArgs),
		continuationCall(FollowupTaskToolName, `{"subagent_id":"`+testSubagentID+`","message":"clarify prior findings"}`),
		{Content: "Group started and earlier task clarified"},
	}}
	groups, followups := 0, 0
	d := groupTestDelegator{enabled: true,
		backgroundTestDelegator: backgroundTestDelegator{continuationTestDelegator: continuationTestDelegator{enabled: true,
			follow: func(_ context.Context, parent DelegationParent, _ FollowupRequest) (RunResult, error) {
				followups++
				return delegationChild(parent, RunCompleted), nil
			},
		}},
		groupSubmit: func(_ context.Context, parent DelegationParent, _ GroupDelegationRequest) (TaskGroup, error) {
			groups++
			return groupReceipt(parent), nil
		},
	}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 3), Request{Input: "research and clarify", ConversationID: 17, Delegator: d})
	if err != nil || groups != 1 || followups != 1 || len(result.State.Children) != 1 || len(result.State.AllCalls()) != 4 {
		t.Fatalf("group consumed unrelated follow-up budget: groups=%d follows=%d state=%+v err=%v", groups, followups, result.State, err)
	}
}

func TestGroupActionsRejectMismatchedBackendIdentity(t *testing.T) {
	for _, name := range []string{GetTaskGroupToolName, CancelTaskGroupToolName} {
		model := &delegationScriptModel{turns: []llm.Turn{continuationCall(name, `{"group_id":"`+testGroupID+`"}`), {Content: "unavailable"}}}
		wrong := func(context.Context, DelegationParent, string) (TaskGroup, error) {
			return TaskGroup{GroupID: testGroupID, ConversationID: 99, Goal: "PRIVATE_GROUP"}, nil
		}
		d := groupTestDelegator{enabled: true, groupGet: wrong, groupCancel: wrong}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "inspect", ConversationID: 7, Delegator: d})
		if err != nil || !strings.Contains(result.State.Steps[0].Error, "ownership mismatch") || strings.Contains(result.State.Steps[0].Result, "PRIVATE_GROUP") {
			t.Fatalf("mismatched group exposed: %+v %v", result.State, err)
		}
	}
}

func TestMissingGroupHandleIsAnErrorAndDoesNotConsumeBudget(t *testing.T) {
	turn := groupCall(validGroupArgs)
	turn.ToolCalls = append(turn.ToolCalls, turn.ToolCalls...)
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
	starts := 0
	d := groupTestDelegator{enabled: true, groupSubmit: func(_ context.Context, parent DelegationParent, _ GroupDelegationRequest) (TaskGroup, error) {
		starts++
		if starts == 1 {
			return TaskGroup{}, nil
		}
		return groupReceipt(parent), nil
	}}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "research", ConversationID: 9, Delegator: d})
	if err != nil || starts != 2 || !strings.Contains(result.State.Steps[0].Error, "did not produce a group") || result.State.Steps[1].Error != "" {
		t.Fatalf("empty handle became success or consumed budget: %+v %v", result.State, err)
	}
}
