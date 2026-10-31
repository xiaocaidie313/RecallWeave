package subagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

type managerTestModel struct {
	next func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error)
}

func (m *managerTestModel) Next(ctx context.Context, conversation *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
	return m.next(ctx, conversation, tools)
}

func (*managerTestModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", errors.New("research delegation must use the tool loop")
}

func managerParent(model llm.ChatGenerator, tools ...llm.Tool) runtime.DelegationParent {
	return runtime.DelegationParent{
		RunID: "parent-run", ConversationID: 17,
		Agent: agent.NewAgent(1, "parent", model, "PRIVATE_PARENT_ROLE", 6,
			agent.NewAbility("parent-tools", "parent tools", "PRIVATE_PARENT_INSTRUCTIONS", tools)),
	}
}

func managerReadTool(name string, run llm.ToolFunc) llm.Tool {
	return llm.Tool{Schema: llm.ToolSchema{Name: name}, Run: run}
}

func testManager(t *testing.T, config Config) *Manager {
	t.Helper()
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestResearcherRetainsExternalToolInstructions(t *testing.T) {
	parent := agent.NewAgent(1, "parent", nil, "", 6,
		agent.NewAbility("memory", "", "", agent.ToolBox{managerReadTool("get_messages", nil)}))
	parent.ExternalTools = []llm.Tool{managerReadTool("mcp_tavily_search", nil), managerReadTool("mcp_write_file", nil)}
	manager := testManager(t, Config{})
	child := manager.newChild(parent, manager.PresetAgents[manager.DefaultAgent()])
	if len(child.ExternalTools) != 1 || child.ExternalTools[0].Schema.Name != "mcp_tavily_search" {
		t.Fatalf("allowed remote reader lost its external classification: %+v", child.ExternalTools)
	}
	instructions := child.Instructions()
	for _, rule := range []string{"外部工具与资料规则", "不发送完整聊天、记忆原文、密钥", "不可信资料"} {
		if !strings.Contains(instructions, rule) {
			t.Fatalf("child lost external-tool policy %q", rule)
		}
	}
	if len(parent.ExternalTools) != 2 {
		t.Fatal("shared parent external tools were mutated")
	}
}

func TestDelegateRunsIndependentResearchLoop(t *testing.T) {
	var childConversation *llm.Conversation
	modelCalls, toolCalls := 0, 0
	model := &managerTestModel{next: func(_ context.Context, conversation *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
		modelCalls++
		if childConversation == nil {
			childConversation = conversation
		} else if childConversation != conversation {
			t.Fatal("one child Run changed its conversation between model turns")
		}
		if conversation.ID != 17 {
			t.Fatalf("conversation association lost: %d", conversation.ID)
		}
		for _, tool := range tools {
			if tool.Name == runtime.DelegateTaskToolName {
				t.Fatal("child exposes recursive delegation")
			}
		}
		if modelCalls == 1 {
			return llm.Turn{
				ToolCalls: []llm.ToolCall{{ID: "read", Name: "get_messages", Arguments: `{"session_id":8,"seq_start":1,"seq_end":1}`}},
				Usage:     llm.Usage{PromptTokens: 10, CompletionTokens: 2},
			}, nil
		}
		return llm.Turn{Content: "结论来自会话 8 消息 1。", Usage: llm.Usage{PromptTokens: 20, CompletionTokens: 5}}, nil
	}}
	parent := managerParent(model, managerReadTool("get_messages", func(_ context.Context, arguments string) (string, error) {
		toolCalls++
		if arguments != `{"session_id":8,"seq_start":1,"seq_end":1}` {
			t.Fatalf("tool arguments changed: %s", arguments)
		}
		return `{"session_id":8,"messages":[{"seq":1,"role":"user","content":"使用 Go"}]}`, nil
	}))
	request := runtime.DelegationRequest{Task: "核查后端语言", Context: "只核查指定来源", ExpectedOutput: "结论和消息位置"}
	result, err := testManager(t, Config{}).Delegate(context.Background(), parent, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Status != runtime.RunCompleted || result.Content == "" || modelCalls != 2 || toolCalls != 1 {
		t.Fatalf("delegation did not execute the full loop: %+v model=%d tool=%d", result, modelCalls, toolCalls)
	}
	if result.State.RunID == "" || result.State.RunID == parent.RunID || result.State.ParentRunID != parent.RunID || result.State.ConversationID != parent.ConversationID || result.State.AgentName != "researcher" {
		t.Fatalf("incorrect child identity: %+v", result.State)
	}
	if result.State.Input != taskInput(request) || strings.Contains(result.State.Input, "PRIVATE_PARENT") {
		t.Fatalf("unexpected child input: %q", result.State.Input)
	}
	if len(result.State.Steps) != 1 || len(result.State.Calls) != 2 || len(result.State.Children) != 0 || result.State.FinishedAt == nil {
		t.Fatalf("child execution not independently recorded: %+v", result.State)
	}
	for _, call := range result.State.Calls {
		if call.RunID != result.State.RunID {
			t.Fatalf("child call attributed to another Run: %+v", call)
		}
	}
	if len(parent.Agent.Abilities[0].ToolBox) != 1 || parent.Agent.Name != "parent" || parent.Agent.MaxRounds != 6 {
		t.Fatal("manager modified shared parent configuration")
	}
}

func TestDelegateEnforcesReadOnlyToolScopeEvenForUnexpectedCalls(t *testing.T) {
	allowedNames := []string{"get_messages", "get_sessions", "mcp_tavily_extract", "mcp_tavily_search", "search_memories"}
	blockedNames := []string{"save_artifact", "deactivate_memory", "mcp_search_and_delete", runtime.DelegateTaskToolName, "load_skill"}
	toolCalls := make(map[string]int)
	var parentTools []llm.Tool
	for _, name := range append(slices.Clone(allowedNames), blockedNames...) {
		parentTools = append(parentTools, managerReadTool(name, func(context.Context, string) (string, error) {
			toolCalls[name]++
			return `{}`, nil
		}))
	}
	modelCalls := 0
	model := &managerTestModel{next: func(_ context.Context, _ *llm.Conversation, schemas []llm.ToolSchema) (llm.Turn, error) {
		modelCalls++
		var names []string
		for _, schema := range schemas {
			names = append(names, schema.Name)
		}
		slices.Sort(names)
		expected := append(slices.Clone(allowedNames), "update_plan")
		slices.Sort(expected)
		if !slices.Equal(names, expected) {
			t.Fatalf("unexpected child tool scope: %v", names)
		}
		if modelCalls > 1 {
			return llm.Turn{Content: "完成，只读工具以外的调用均被拒绝。"}, nil
		}
		var calls []llm.ToolCall
		for _, name := range append(slices.Clone(allowedNames), blockedNames...) {
			calls = append(calls, llm.ToolCall{ID: name, Name: name, Arguments: `{}`})
		}
		return llm.Turn{ToolCalls: calls}, nil
	}}
	result, err := testManager(t, Config{}).Delegate(context.Background(), managerParent(model, parentTools...), runtime.DelegationRequest{Task: "read only"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range allowedNames {
		if toolCalls[name] != 1 {
			t.Fatalf("allowed tool %s was not called once: %v", name, toolCalls)
		}
	}
	for _, name := range blockedNames {
		if toolCalls[name] != 0 {
			t.Fatalf("blocked parent tool %s executed", name)
		}
	}
	if len(result.State.Errors) != len(blockedNames) || len(result.State.Children) != 0 {
		t.Fatalf("unexpected calls were not contained: %+v", result.State)
	}
}

func TestDelegatePreservesBranchSnapshotReadScope(t *testing.T) {
	modelCalls := 0
	model := &managerTestModel{next: func(_ context.Context, _ *llm.Conversation, schemas []llm.ToolSchema) (llm.Turn, error) {
		modelCalls++
		for _, schema := range schemas {
			if schema.Name == "search_memories" || schema.Name == "get_sessions" {
				t.Fatalf("branch regained global reader: %s", schema.Name)
			}
		}
		switch modelCalls {
		case 1:
			return llm.Turn{ToolCalls: []llm.ToolCall{
				{ID: "search", Name: "search_memories", Arguments: `{"keyword":"future"}`},
				{ID: "prefix", Name: "get_messages", Arguments: `{"session_id":10,"seq_start":1,"seq_end":3}`},
			}}, nil
		case 2:
			return llm.Turn{ToolCalls: []llm.ToolCall{
				{ID: "future", Name: "get_messages", Arguments: `{"session_id":10,"seq_start":2,"seq_end":3}`},
				{ID: "other", Name: "get_messages", Arguments: `{"session_id":99,"seq_start":1,"seq_end":1}`},
			}}, nil
		default:
			return llm.Turn{Content: "只有分叉前的原文可用。"}, nil
		}
	}}
	parent := managerParent(model)
	parent.Agent.Abilities = []*agent.Ability{ability.NewBranchContextAbility([]store.ContextMessage{
		{SessionID: 10, Seq: 1, Role: "user", Content: "frozen prefix"},
	})}
	result, err := testManager(t, Config{}).Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "核查分支"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.State.Steps) != 4 || len(result.State.Errors) != 2 {
		t.Fatalf("unexpected branch research trace: %+v", result.State)
	}
	if !strings.Contains(result.State.Steps[1].Result, "frozen prefix") || strings.Contains(result.State.Steps[1].Result, `"seq":2`) {
		t.Fatalf("snapshot prefix was not preserved: %s", result.State.Steps[1].Result)
	}
	if !strings.Contains(result.State.Steps[2].Result, `"messages":[]`) || result.State.Steps[3].Error == "" {
		t.Fatalf("child escaped snapshot limits: %+v", result.State.Steps)
	}
}

func TestDelegateTimeoutPreservesFailedRunAndUsage(t *testing.T) {
	model := &managerTestModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		<-ctx.Done()
		return llm.Turn{Usage: llm.Usage{PromptTokens: 9}}, ctx.Err()
	}}
	result, err := testManager(t, Config{Timeout: 10 * time.Millisecond}).Delegate(context.Background(), managerParent(model), runtime.DelegationRequest{Task: "等待模型"})
	if !errors.Is(err, context.DeadlineExceeded) || result.State.Status != runtime.RunCancelled || result.State.RunID == "" || result.State.ParentRunID != "parent-run" {
		t.Fatalf("timeout lost child state: %+v err=%v", result, err)
	}
	if len(result.State.Calls) != 1 || result.State.Calls[0].Usage.PromptTokens != 9 || result.State.FinishedAt == nil {
		t.Fatalf("timeout lost actual usage or completion: %+v", result.State)
	}
}

func TestDelegatePropagatesParentCancellationToChildTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	model := &managerTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "wait", Name: "get_messages", Arguments: `{}`}}}, nil
	}}
	parent := managerParent(model, managerReadTool("get_messages", func(ctx context.Context, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	type outcome struct {
		result runtime.RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := testManager(t, Config{Timeout: time.Second}).Delegate(ctx, parent, runtime.DelegationRequest{Task: "等待原文"})
		done <- outcome{result, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("child did not start its tool")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result.State.Status != runtime.RunCancelled || len(got.result.State.Steps) != 1 || got.result.State.Steps[0].Error == "" {
			t.Fatalf("parent cancellation was not retained: %+v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop child tool")
	}
}

func TestDelegateEnforcesChildRoundLimit(t *testing.T) {
	var calls int
	model := &managerTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls++
		return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "read", Name: "get_messages", Arguments: `{}`}}}, nil
	}}
	parent := managerParent(model, managerReadTool("get_messages", func(context.Context, string) (string, error) { return `{}`, nil }))
	result, err := testManager(t, Config{MaxRounds: 2}).Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "不会结束的查询"})
	if err == nil || !strings.Contains(err.Error(), "maximum tool rounds") || result.State.Status != runtime.RunFailed || calls != 2 || len(result.State.Calls) != 2 || len(result.State.Steps) != 2 {
		t.Fatalf("child round budget not enforced: %+v err=%v calls=%d", result, err, calls)
	}
}

func TestManagerConcurrentDelegationsKeepIndependentRunsAndScopes(t *testing.T) {
	const workers = 8
	var mu sync.Mutex
	conversations := make(map[*llm.Conversation]int)
	model := &managerTestModel{next: func(_ context.Context, conversation *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		mu.Lock()
		conversations[conversation]++
		turn := conversations[conversation]
		mu.Unlock()
		if turn == 1 {
			return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "read", Name: "get_messages", Arguments: `{}`}}}, nil
		}
		return llm.Turn{Content: "done"}, nil
	}}
	manager := testManager(t, Config{})
	var toolCalls atomic.Int32
	type outcome struct {
		index  int
		result runtime.RunResult
		err    error
	}
	results := make(chan outcome, workers)
	for i := range workers {
		go func() {
			parent := managerParent(model, managerReadTool("get_messages", func(context.Context, string) (string, error) {
				toolCalls.Add(1)
				return fmt.Sprintf(`{"scope":%d}`, i), nil
			}))
			parent.RunID = fmt.Sprintf("parent-%d", i)
			parent.ConversationID = uint(i + 1)
			result, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: fmt.Sprintf("task-%d", i)})
			results <- outcome{i, result, err}
		}()
	}
	seenIDs := make(map[string]bool)
	for range workers {
		got := <-results
		if got.err != nil || got.result.State.Status != runtime.RunCompleted {
			t.Fatalf("concurrent task failed: %+v err=%v", got.result, got.err)
		}
		state := got.result.State
		if state.ParentRunID != fmt.Sprintf("parent-%d", got.index) || state.ConversationID != uint(got.index+1) || len(state.Calls) != 2 || len(state.Steps) != 1 || state.Steps[0].Result != fmt.Sprintf(`{"scope":%d}`, got.index) {
			t.Fatalf("concurrent state or tool scope mixed: %+v", state)
		}
		if state.RunID == "" || seenIDs[state.RunID] {
			t.Fatalf("child Run identity reused: %q", state.RunID)
		}
		seenIDs[state.RunID] = true
	}
	if len(conversations) != workers || toolCalls.Load() != workers {
		t.Fatalf("shared manager mixed child execution: conversations=%d tools=%d", len(conversations), toolCalls.Load())
	}
}

func TestDelegateRejectsMissingParentAndTask(t *testing.T) {
	model := &managerTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		t.Fatal("invalid delegation started a model request")
		return llm.Turn{}, nil
	}}
	valid := managerParent(model)
	tests := []struct {
		name    string
		manager *Manager
		parent  runtime.DelegationParent
		task    string
	}{
		{"nil manager", nil, valid, "task"},
		{"nil agent", testManager(t, Config{}), runtime.DelegationParent{RunID: "parent"}, "task"},
		{"nil model", testManager(t, Config{}), managerParent(nil), "task"},
		{"no parent run", testManager(t, Config{}), runtime.DelegationParent{Agent: valid.Agent}, "task"},
		{"empty task", testManager(t, Config{}), valid, " \n "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.manager.Delegate(context.Background(), test.parent, runtime.DelegationRequest{Task: test.task})
			if err == nil || result.State.RunID != "" {
				t.Fatalf("invalid delegation accepted: %+v err=%v", result, err)
			}
		})
	}
}
