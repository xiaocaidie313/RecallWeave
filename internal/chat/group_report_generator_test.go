package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/delivery"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

func groupReportFixture(t *testing.T) (store.TaskGroup, []store.SubagentTask) {
	t.Helper()
	first := autoReportFixture(t)
	var child runtime.RunResult
	if err := json.Unmarshal(first.Result, &child); err != nil {
		t.Fatal(err)
	}
	child.State.ConversationID = 9
	first.ID, first.GroupID, first.ConversationID = "task-a", "group-id", 9
	first.Result, _ = json.Marshal(child)
	second := store.SubagentTask{ID: "task-b", GroupID: "group-id", ConversationID: 9, ParentRunID: first.ParentRunID, SubagentID: "child-b", AgentName: "researcher", Task: "check external evidence", Status: "failed", Error: "provider denied request"}
	ids, _ := json.Marshal([]string{first.ID, second.ID})
	return store.TaskGroup{ID: "group-id", ConversationID: 9, ParentRunID: first.ParentRunID, Goal: "compare personal and external evidence", ExpectedOutput: "cite sources and show contradictions", DeliveryMode: delivery.Synthesize, TaskIDs: ids, Status: "partial"}, []store.SubagentTask{first, second}
}

func TestGenerateGroupReportCombinesSuccessfulEvidenceAndFailedMemberInOneRun(t *testing.T) {
	group, tasks := groupReportFixture(t)
	model := &reportTestModel{next: func(_ context.Context, conversation *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
		if len(tools) != 0 {
			t.Fatalf("group report exposed tools: %+v", tools)
		}
		snapshot, err := conversation.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		text := string(snapshot.Messages)
		for _, fragment := range []string{group.Goal, group.ExpectedOutput, "KNOWN_FINDING", "provider denied request", "task-a", "task-b", "https://example.com/source"} {
			if !strings.Contains(text, fragment) {
				t.Errorf("missing group evidence %q", fragment)
			}
		}
		for _, private := range []string{"PRIVATE_CHILD_HISTORY", "context_snapshot", "GLOBAL_MAIN_POLICY"} {
			if strings.Contains(text, private) {
				t.Errorf("private child/main context leaked: %s", private)
			}
		}
		return llm.Turn{Content: "保留历史调查成果；外部调查失败，外部结论仍缺证据。", Usage: llm.Usage{PromptTokens: 13, CompletionTokens: 4}}, nil
	}}
	core := agent.NewAgent(1, "recall", model, "GLOBAL_MAIN_POLICY", 6)
	report, err := NewAgent(core, nil, nil).GenerateGroupReport(context.Background(), group, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || report.Run == nil || report.Run.PromptTokens != 13 || report.Run.ConversationID != 9 || report.Run.SubagentID != "" || report.Run.RunID == tasks[0].RunID || report.Message.Role != "assistant" || !strings.Contains(report.Message.Content, "失败") {
		t.Fatalf("wrong group report/accounting: %+v calls=%d", report, model.calls)
	}
	var state runtime.RunState
	if err := json.Unmarshal(report.Run.State, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Calls) != 1 || len(state.Children) != 0 || len(state.Steps) != 0 || len(state.InheritedSources) != 1 || state.InheritedSources[0].SourceRunID != "child-run" {
		t.Fatalf("group billing/provenance changed: %+v", state)
	}
	if len(report.Sources) != 1 || report.Sources[0] != state.InheritedSources[0] {
		t.Fatalf("group report delivery discarded source metadata: %+v", report.Sources)
	}
}

func TestGenerateGroupReportCombinedPreservesOriginalsInDeclaredOrderWithoutModel(t *testing.T) {
	for _, mode := range []string{"", delivery.Combined} {
		t.Run("mode="+mode, func(t *testing.T) {
			group, tasks := groupReportFixture(t)
			group.DeliveryMode, group.Status = mode, "completed"
			var child runtime.RunResult
			if err := json.Unmarshal(tasks[0].Result, &child); err != nil {
				t.Fatal(err)
			}
			original := strings.Repeat("完整原文", 8<<10) + "FIRST_RESULT_TAIL"
			child.Content = original
			tasks[0].Result, _ = json.Marshal(child)
			child.Content, child.State.RunID, child.State.SubagentID = "SECOND_RESULT", "child-run-b", "child-b"
			child.State.InheritedSources = []runtime.Source{{Kind: "web", Tool: "mcp_tavily_extract", SourceRunID: "child-run-b", URL: "https://example.com/second"}}
			tasks[1].Status, tasks[1].Error, tasks[1].RunID = "completed", "", child.State.RunID
			tasks[1].Result, _ = json.Marshal(child)
			model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
				t.Fatal("combined delivery called model")
				return llm.Turn{}, nil
			}}
			report, err := NewAgent(agent.NewAgent(1, "recall", model, "", 4), nil, nil).GenerateGroupReport(context.Background(), group, []store.SubagentTask{tasks[1], tasks[0]})
			if err != nil {
				t.Fatal(err)
			}
			text := report.Message.Content
			if model.calls != 0 || report.Run != nil || report.Error != "" || !strings.Contains(text, original) || !strings.Contains(text, child.Content) || strings.Index(text, "FIRST_RESULT_TAIL") > strings.Index(text, "SECOND_RESULT") || strings.Contains(text, "内容截断") {
				t.Fatalf("combined result rewritten, truncated or reordered: report=%+v calls=%d", report, model.calls)
			}
			if len(report.Sources) != 2 || report.Sources[0].SourceRunID != "child-run" || report.Sources[1].SourceRunID != "child-run-b" {
				t.Fatalf("combined source provenance lost: %+v", report.Sources)
			}
		})
	}
}

func TestGenerateGroupReportCombinedPartialWorksWithoutConfiguredModel(t *testing.T) {
	group, tasks := groupReportFixture(t)
	group.DeliveryMode = delivery.Combined
	report, err := NewAgent(nil, nil, nil).GenerateGroupReport(context.Background(), group, tasks)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"KNOWN_FINDING", "正常结束", "执行失败", "provider denied request"} {
		if !strings.Contains(report.Message.Content, fragment) {
			t.Fatalf("combined delivery lost %q: %+v", fragment, report)
		}
	}
	if report.Run != nil || report.Error != "" || len(report.Sources) != 1 {
		t.Fatalf("combined delivery unexpectedly required model: %+v", report)
	}
}

func TestGenerateGroupReportRejectsDirectAndUnknownModesWithoutModel(t *testing.T) {
	for _, mode := range []string{delivery.Direct, "automatic"} {
		group, tasks := groupReportFixture(t)
		group.DeliveryMode = mode
		model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
			t.Fatal("invalid group delivery reached model")
			return llm.Turn{}, nil
		}}
		if _, err := NewAgent(agent.NewAgent(1, "recall", model, "", 4), nil, nil).GenerateGroupReport(context.Background(), group, tasks); err == nil {
			t.Fatalf("group delivery mode %q accepted", mode)
		}
		if model.calls != 0 {
			t.Fatal("rejected delivery consumed model")
		}
	}
}

func TestGenerateGroupReportCombinedRejectsOversizedOriginalInsteadOfTruncating(t *testing.T) {
	group, tasks := groupReportFixture(t)
	group.DeliveryMode = delivery.Combined
	var child runtime.RunResult
	if err := json.Unmarshal(tasks[0].Result, &child); err != nil {
		t.Fatal(err)
	}
	child.Content = strings.Repeat("x", 512<<10)
	tasks[0].Result, _ = json.Marshal(child)
	if _, err := NewAgent(nil, nil, nil).GenerateGroupReport(context.Background(), group, tasks); err == nil || !strings.Contains(err.Error(), "storage limit") {
		t.Fatalf("oversized combined result silently lost: %v", err)
	}
}

func TestGenerateGroupReportRetainsBothSuccessfulSourceSets(t *testing.T) {
	group, tasks := groupReportFixture(t)
	group.Status = "completed"
	var child runtime.RunResult
	if err := json.Unmarshal(tasks[0].Result, &child); err != nil {
		t.Fatal(err)
	}
	child.Content = "SECOND_FINDING"
	child.State.RunID = "child-run-b"
	child.State.SubagentID = "child-b"
	child.State.InheritedSources = []runtime.Source{{Kind: "web", Tool: "mcp_tavily_extract", SourceRunID: "child-run-b", URL: "https://example.com/second"}}
	tasks[1].Status, tasks[1].Error, tasks[1].RunID = "completed", "", child.State.RunID
	tasks[1].Result, _ = json.Marshal(child)
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "两路资料对照完成。"}, nil
	}}
	report, err := NewAgent(agent.NewAgent(1, "recall", model, "", 4), nil, nil).GenerateGroupReport(context.Background(), group, []store.SubagentTask{tasks[1], tasks[0]})
	if err != nil {
		t.Fatal(err)
	}
	var state runtime.RunState
	if err := json.Unmarshal(report.Run.State, &state); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || len(state.InheritedSources) != 2 || state.InheritedSources[0].SourceRunID != "child-run" || state.InheritedSources[1].SourceRunID != "child-run-b" {
		t.Fatalf("source sets lost or reordered: %+v", state.InheritedSources)
	}
}

func TestGenerateGroupReportNoUsableResultDoesNotCallModel(t *testing.T) {
	group, tasks := groupReportFixture(t)
	group.Status = "failed"
	tasks[0].Status, tasks[0].Error, tasks[0].Result = "interrupted", "service stopped", nil
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		t.Fatal("failed group called model")
		return llm.Turn{}, nil
	}}
	report, err := NewAgent(agent.NewAgent(1, "recall", model, "", 4), nil, nil).GenerateGroupReport(context.Background(), group, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 0 || report.Run != nil || !strings.Contains(report.Message.Content, "执行中断") || !strings.Contains(report.Message.Content, "执行失败") || !strings.Contains(report.Message.Content, "provider denied request") {
		t.Fatalf("missing deterministic failure report: %+v", report)
	}
}

func TestGenerateGroupReportFailureKeepsRunAndDoesNotRetry(t *testing.T) {
	group, tasks := groupReportFixture(t)
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Usage: llm.Usage{PromptTokens: 17}}, errors.New("model timeout")
	}}
	report, err := NewAgent(agent.NewAgent(1, "recall", model, "", 4), nil, nil).GenerateGroupReport(context.Background(), group, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || report.Run == nil || report.Run.Status != "failed" || report.Run.PromptTokens != 17 || len(report.Sources) != 1 || !strings.Contains(report.Message.Content, "综合整理失败") || !strings.Contains(report.Message.Content, "KNOWN_FINDING") || !strings.Contains(report.Message.Content, "provider denied request") || !strings.Contains(report.Error, "model timeout") {
		t.Fatalf("group failure lost or retried: %+v", report)
	}
}

func TestGenerateGroupReportRejectsIncompleteForeignOrWrongMembership(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "foreign", "wrong-group", "running-member", "wrong-result", "wrong-parent", "running-group"} {
		t.Run(name, func(t *testing.T) {
			group, tasks := groupReportFixture(t)
			switch name {
			case "missing":
				tasks = tasks[:1]
			case "duplicate":
				tasks[1] = tasks[0]
			case "foreign":
				tasks[1].ConversationID = 10
			case "wrong-group":
				tasks[1].GroupID = "another"
			case "running-member":
				tasks[1].Status = "running"
			case "wrong-result":
				tasks[0].RunID = "other-run"
			case "wrong-parent":
				tasks[1].ParentRunID = "another-parent"
			case "running-group":
				group.Status = "running"
			}
			model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
				t.Fatal("invalid group reached model")
				return llm.Turn{}, nil
			}}
			if _, err := NewAgent(agent.NewAgent(1, "recall", model, "", 4), nil, nil).GenerateGroupReport(context.Background(), group, tasks); err == nil {
				t.Fatal("invalid group accepted")
			}
		})
	}
}
