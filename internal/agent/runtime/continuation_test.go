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

const testSubagentID = "0123456789abcdef0123456789abcdef"

type continuationTestDelegator struct {
	runtimeTestDelegator
	enabled bool
	follow  func(context.Context, DelegationParent, FollowupRequest) (RunResult, error)
	list    func(context.Context, DelegationParent) ([]SubagentInfo, error)
}

func (d continuationTestDelegator) ContinuationsEnabled() bool { return d.enabled }
func (d continuationTestDelegator) FollowUp(ctx context.Context, parent DelegationParent, request FollowupRequest) (RunResult, error) {
	return d.follow(ctx, parent, request)
}
func (d continuationTestDelegator) ListSubagents(ctx context.Context, parent DelegationParent) ([]SubagentInfo, error) {
	if d.list == nil {
		return nil, nil
	}
	return d.list(ctx, parent)
}

func continuationCall(name, arguments string) llm.Turn {
	return llm.Turn{ToolCalls: []llm.ToolCall{{ID: name, Name: name, Arguments: arguments}}}
}

func TestContinuationToolsOnlyAppearForEnabledParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		parent  string
		want    bool
	}{
		{"parent", true, "", true}, {"disabled", false, "", false}, {"child", true, "parent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &delegationScriptModel{turns: []llm.Turn{{Content: "direct"}}}
			_, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "hi", ParentRunID: tc.parent, Delegator: continuationTestDelegator{enabled: tc.enabled}})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{FollowupTaskToolName, ListSubagentsToolName} {
				found := false
				for _, schema := range model.schemas[0] {
					found = found || schema.Name == name
				}
				if found != tc.want {
					t.Fatalf("%s visible=%v want=%v", name, found, tc.want)
				}
			}
		})
	}
}

func TestListThenFollowupReturnsNewRunAndInheritedSources(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{
		continuationCall(ListSubagentsToolName, `{}`),
		continuationCall(FollowupTaskToolName, `{"subagent_id":"`+testSubagentID+`","message":"check the latest discussion"}`),
		{Content: "continue main answer"},
	}}
	lists, follows := 0, 0
	core := agent.NewAgent(1, "main", model, "", 4)
	d := continuationTestDelegator{enabled: true,
		list: func(_ context.Context, parent DelegationParent) ([]SubagentInfo, error) {
			lists++
			if parent.Agent != core || parent.ConversationID != 9 {
				t.Fatal("listing lost scoped parent")
			}
			return []SubagentInfo{{SubagentID: testSubagentID, Agent: "researcher", Task: "original task", LastRunID: "first-run", Status: RunCompleted, CanFollowUp: true}}, nil
		},
		follow: func(_ context.Context, parent DelegationParent, request FollowupRequest) (RunResult, error) {
			follows++
			if request.SubagentID != testSubagentID || request.Message != "check the latest discussion" {
				t.Fatalf("changed request: %+v", request)
			}
			child := delegationChild(parent, RunCompleted)
			child.State.SubagentID = testSubagentID
			child.State.PreviousRunID = "first-run"
			child.State.ContextSnapshot = &llm.ConversationSnapshot{Version: 1, Messages: json.RawMessage(`[]`)}
			child.State.InheritedSources = []Source{{SourceRunID: "first-run", Kind: "message", Tool: "get_messages", SessionID: 7, SeqStart: 4, SeqEnd: 4}}
			child.State.Steps = []RunStep{{ToolName: "get_messages", Result: `{"session_id":7,"messages":[{"seq":5,"content":"new original"}]}`}}
			return child, nil
		},
	}
	result, err := Run(context.Background(), core, Request{Input: "continue task", ConversationID: 9, Delegator: d})
	if err != nil {
		t.Fatal(err)
	}
	if lists != 1 || follows != 1 || len(result.State.Children) != 1 || len(result.State.AllCalls()) != 4 {
		t.Fatalf("unexpected lifecycle: lists=%d follows=%d state=%+v", lists, follows, result.State)
	}
	var listing struct {
		Subagents []SubagentInfo `json:"subagents"`
	}
	if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &listing); err != nil || len(listing.Subagents) != 1 || listing.Subagents[0].SubagentID != testSubagentID {
		t.Fatalf("invalid listing: %+v %v", listing, err)
	}
	var reply DelegationResult
	if err := json.Unmarshal([]byte(result.State.Steps[1].Result), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.SubagentID != testSubagentID || !reply.CanFollowUp || reply.Calls != 1 || reply.Usage.PromptTokens != 13 || len(reply.Sources) != 2 {
		t.Fatalf("bad follow-up result: %+v", reply)
	}
	if reply.Sources[0].SourceRunID != reply.ChildRunID || reply.Sources[1].SourceRunID != "first-run" {
		t.Fatalf("source provenance was overwritten: %+v", reply.Sources)
	}
}

func TestFollowupUnknownTargetCanBeCorrectedAndActualRunConsumesLimit(t *testing.T) {
	turn := continuationCall(FollowupTaskToolName, `{"subagent_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","message":"check"}`)
	for _, id := range []string{testSubagentID, testSubagentID} {
		turn.ToolCalls = append(turn.ToolCalls, continuationCall(FollowupTaskToolName, `{"subagent_id":"`+id+`","message":"check"}`).ToolCalls...)
	}
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
	starts := 0
	d := continuationTestDelegator{enabled: true, follow: func(_ context.Context, parent DelegationParent, request FollowupRequest) (RunResult, error) {
		starts++
		if request.SubagentID != testSubagentID {
			return RunResult{}, errors.New("subagent not found in this conversation")
		}
		child := delegationChild(parent, RunFailed)
		child.State.SubagentID = testSubagentID
		return child, errors.New("child model failed")
	}}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "continue", Delegator: d})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 2 || len(result.State.Children) != 1 || result.State.Steps[0].Error == "" || result.State.Steps[1].Error != "" || !strings.Contains(result.State.Steps[2].Error, "one follow-up") {
		t.Fatalf("wrong follow-up budget: starts=%d state=%+v", starts, result.State)
	}
	var reply DelegationResult
	if err := json.Unmarshal([]byte(result.State.Steps[1].Result), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Status != RunFailed || reply.Error != "child model failed" || reply.Usage.PromptTokens != 13 || reply.CanFollowUp {
		t.Fatalf("failed Run lost its outcome: %+v", reply)
	}
}

func TestDelegateAndFollowupHaveSeparateBoundedBudgets(t *testing.T) {
	model := &delegationScriptModel{turns: []llm.Turn{
		delegationCall(`{"task":"first investigation"}`),
		continuationCall(FollowupTaskToolName, `{"subagent_id":"`+testSubagentID+`","message":"check missing part"}`),
		{Content: "done"},
	}}
	starts, follows := 0, 0
	d := continuationTestDelegator{enabled: true,
		runtimeTestDelegator: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
			starts++
			child := delegationChild(parent, RunCompleted)
			child.State.SubagentID = testSubagentID
			return child, nil
		},
		follow: func(_ context.Context, parent DelegationParent, _ FollowupRequest) (RunResult, error) {
			follows++
			child := delegationChild(parent, RunCompleted)
			child.State.RunID += "_followup"
			child.State.SubagentID = testSubagentID
			return child, nil
		},
	}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 3), Request{Input: "investigate", Delegator: d})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || follows != 1 || len(result.State.Children) != 2 || len(result.State.AllCalls()) != 5 {
		t.Fatalf("wrong combined budget: starts=%d follows=%d state=%+v", starts, follows, result.State)
	}
}

func TestFollowupRejectsInvalidArgumentsBeforeManager(t *testing.T) {
	for _, arguments := range []string{
		"", `null`, `[]`, `{}`, `{"subagent_id":"` + testSubagentID + `","message":" "}`,
		`{"subagent_id":"` + strings.ToUpper(testSubagentID) + `","message":"check"}`,
		`{"subagent_id":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz","message":"check"}`,
		`{"subagent_id":"short","message":"check"}`,
		`{"subagent_id":"` + testSubagentID + `","message":3}`,
		`{"subagent_id":"` + testSubagentID + `","message":"check","agent":"researcher"}`,
		`{"subagent_id":"` + testSubagentID + `","message":"check"}{}`,
		`{"subagent_id":"` + testSubagentID + `","message":"` + strings.Repeat("x", (8<<10)+1) + `"}`,
		"{\"subagent_id\":\"" + testSubagentID + "\",\"message\":\"" + string([]byte{0xff}) + "\"}",
	} {
		model := &delegationScriptModel{turns: []llm.Turn{continuationCall(FollowupTaskToolName, arguments), {Content: "invalid input"}}}
		d := continuationTestDelegator{enabled: true, follow: func(context.Context, DelegationParent, FollowupRequest) (RunResult, error) {
			t.Fatal("invalid request reached manager")
			return RunResult{}, nil
		}}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "continue", Delegator: d})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.State.Children) != 0 || result.State.Steps[0].Error == "" {
			t.Fatalf("invalid arguments accepted: %q", arguments)
		}
	}
}

func TestListSubagentsAcceptsOnlyEmptyArguments(t *testing.T) {
	for _, arguments := range []string{"", `{}`, `null`, `[]`, `{"conversation_id":9}`, `{}{}`} {
		want := arguments == "" || arguments == `{}`
		calls := 0
		d := continuationTestDelegator{enabled: true, list: func(context.Context, DelegationParent) ([]SubagentInfo, error) { calls++; return nil, nil }}
		model := &delegationScriptModel{turns: []llm.Turn{continuationCall(ListSubagentsToolName, arguments), {Content: "done"}}}
		result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "list", Delegator: d})
		if err != nil {
			t.Fatal(err)
		}
		if (calls == 1) != want || (result.State.Steps[0].Error == "") != want || len(result.State.Children) != 0 {
			t.Fatalf("list args=%q calls=%d steps=%+v", arguments, calls, result.State.Steps)
		}
	}
}

func TestContinuationReservedNamesCannotBeBusinessTools(t *testing.T) {
	for _, name := range []string{FollowupTaskToolName, ListSubagentsToolName} {
		model := &delegationScriptModel{}
		core := agent.NewAgent(1, "main", model, "", 2, agent.NewAbility("business", "", "", agent.ToolBox{{Schema: llm.ToolSchema{Name: name}}}))
		result, err := Run(context.Background(), core, Request{Input: "hi"})
		if err == nil || model.calls != 0 || result.State.Status != RunFailed {
			t.Fatalf("reserved tool %s accepted: err=%v state=%+v", name, err, result.State)
		}
	}
}

type snapshotRuntimeModel struct {
	turns  []llm.Turn
	calls  int
	before func(*llm.Conversation)
}

func (*snapshotRuntimeModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected one-turn call")
}
func (m *snapshotRuntimeModel) Next(_ context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
	if m.before != nil {
		m.before(conv)
	}
	if m.calls >= len(m.turns) {
		return llm.Turn{}, errors.New("unexpected model call")
	}
	turn := m.turns[m.calls]
	m.calls++
	conv.AddAssistantTurn(turn)
	return turn, nil
}

func TestResumedRunRestoresHistoryWithoutReplayingToolsOrCountingOldUsage(t *testing.T) {
	reads := 0
	read := agent.NewAbility("read", "", "", agent.ToolBox{{Schema: llm.ToolSchema{Name: "get_messages"}, Run: func(context.Context, string) (string, error) {
		reads++
		return `{"session_id":7,"messages":[{"seq":4,"content":"ORIGINAL_TOOL_EVIDENCE"}]}`, nil
	}}})
	firstModel := &snapshotRuntimeModel{turns: []llm.Turn{
		{ToolCalls: []llm.ToolCall{{ID: "read_original", Name: "get_messages", Arguments: `{"session_id":7,"seq_start":4,"seq_end":4}`}}, Usage: llm.Usage{PromptTokens: 10}},
		{Content: "first findings", Usage: llm.Usage{PromptTokens: 20}},
	}}
	first, err := Run(context.Background(), agent.NewAgent(2, "researcher", firstModel, "OLD_ROLE_POLICY", 3, read), Request{Input: "original task", ParentRunID: "parent-one", SubagentID: testSubagentID, ConversationID: 9, CaptureContext: true})
	if err != nil || first.State.ContextSnapshot == nil || first.State.ContextError != "" {
		t.Fatalf("first checkpoint missing: %+v err=%v", first.State, err)
	}
	encoded, err := json.Marshal(first.State.ContextSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var restored llm.ConversationSnapshot
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	secondModel := &snapshotRuntimeModel{turns: []llm.Turn{{Content: "supplemental answer", Usage: llm.Usage{PromptTokens: 7}}}, before: func(conv *llm.Conversation) {
		checkpoint, err := conv.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		body := string(checkpoint.Messages)
		for _, wanted := range []string{"CURRENT_ROLE_POLICY", "read_original", "ORIGINAL_TOOL_EVIDENCE", "first findings", "follow-up question"} {
			if !strings.Contains(body, wanted) {
				t.Fatalf("restored context lost %q", wanted)
			}
		}
		for _, unwanted := range []string{"OLD_ROLE_POLICY", "PRIVATE_PARENT_PRELOAD"} {
			if strings.Contains(body, unwanted) {
				t.Fatalf("restored context leaked %q", unwanted)
			}
		}
	}}
	second, err := Run(context.Background(), agent.NewAgent(2, "researcher", secondModel, "CURRENT_ROLE_POLICY", 3, read), Request{
		Input: "follow-up question", ParentRunID: "parent-two", SubagentID: testSubagentID, PreviousRunID: first.State.RunID, ConversationID: 9,
		ContextSnapshot: &restored, CaptureContext: true, InheritedSources: SourcesForRun(first.State),
		Preload: Preload{Background: []string{"PRIVATE_PARENT_PRELOAD"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || secondModel.calls != 1 || len(second.State.Calls) != 1 || len(second.State.AllCalls()) != 1 || second.State.Calls[0].Usage.PromptTokens != 7 {
		t.Fatalf("old work was repeated or billed: reads=%d state=%+v", reads, second.State)
	}
	if second.State.SubagentID != first.State.SubagentID || second.State.RunID == first.State.RunID || second.State.PreviousRunID != first.State.RunID || second.State.ParentRunID != "parent-two" || len(second.State.Steps) != 0 || second.State.ContextSnapshot == nil {
		t.Fatalf("incorrect continued identity: %+v", second.State)
	}
	sources := SourcesForRun(second.State)
	if len(sources) != 1 || sources[0].SourceRunID != first.State.RunID {
		t.Fatalf("original evidence provenance lost: %+v", sources)
	}
}

func TestInvalidResumeFailsBeforeModelAndDoesNotRetainCheckpoint(t *testing.T) {
	model := &snapshotRuntimeModel{}
	result, err := Run(context.Background(), agent.NewAgent(2, "researcher", model, "", 2), Request{Input: "continue", CaptureContext: true, ContextSnapshot: &llm.ConversationSnapshot{Version: 999, Messages: json.RawMessage(`[]`)}})
	if err == nil || model.calls != 0 || result.State.Status != RunFailed || result.State.ContextError == "" || result.State.ContextSnapshot != nil {
		t.Fatalf("invalid context reached model: err=%v state=%+v", err, result.State)
	}
}

func TestCancellationCapturesErrorForUnfinishedToolBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &snapshotRuntimeModel{turns: []llm.Turn{{ToolCalls: []llm.ToolCall{{ID: "one", Name: "cancel"}, {ID: "two", Name: "danger"}}, Usage: llm.Usage{PromptTokens: 8}}}}
	danger := 0
	tools := agent.NewAbility("tools", "", "", agent.ToolBox{
		{Schema: llm.ToolSchema{Name: "cancel"}, Run: func(context.Context, string) (string, error) { cancel(); return "cancelled", nil }},
		{Schema: llm.ToolSchema{Name: "danger"}, Run: func(context.Context, string) (string, error) { danger++; return "executed", nil }},
	})
	result, err := Run(ctx, agent.NewAgent(2, "researcher", model, "", 2, tools), Request{Input: "investigate", CaptureContext: true, SubagentID: testSubagentID})
	if !errors.Is(err, context.Canceled) || danger != 0 || result.State.Status != RunCancelled || result.State.ContextError == "" || result.State.ContextSnapshot != nil || len(result.State.Calls) != 1 {
		t.Fatalf("incomplete tools became resumable: err=%v state=%+v", err, result.State)
	}
}
