package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/delivery"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/types"
	"recallweave/internal/watch/useage"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type reportTestModel struct {
	calls int
	next  func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error)
}

func (m *reportTestModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected extra model call")
}
func (m *reportTestModel) Next(ctx context.Context, conversation *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
	m.calls++
	return m.next(ctx, conversation, tools)
}

func autoReportFixture(t *testing.T) types.SubagentTask {
	t.Helper()
	child := runtime.RunResult{Content: "KNOWN_FINDING", State: runtime.RunState{RunID: "child-run", SubagentID: "child-id", ConversationID: 7, Status: runtime.RunCompleted,
		ContextSnapshot:  &llm.ConversationSnapshot{Version: 1, Messages: json.RawMessage(`[{"role":"user","content":"PRIVATE_CHILD_HISTORY"}]`)},
		Calls:            []useage.ModelCallStats{{Usage: useage.TokenUsage{PromptTokens: 9000}}},
		InheritedSources: []runtime.Source{{Kind: "web", Tool: "mcp_tavily_extract", SourceRunID: "child-run", URL: "https://example.com/source", Title: "actual source"}},
	}}
	encoded, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	return types.SubagentTask{ID: "task-id", SubagentID: "child-id", RunID: "child-run", ParentRunID: "original-parent", ConversationID: 7, AgentName: "researcher", Task: "check history", DeliveryMode: delivery.Synthesize, Status: "completed", Result: encoded}
}

func TestGenerateReportUsesOneIsolatedMainRunAndVerifiedSources(t *testing.T) {
	model := &reportTestModel{next: func(_ context.Context, conversation *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
		if len(tools) != 0 {
			t.Fatalf("report exposed tools: %+v", tools)
		}
		snapshot, err := conversation.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		messages := string(snapshot.Messages)
		for _, expected := range []string{"KNOWN_FINDING", "https://example.com/source", "actual source", "task-id"} {
			if !strings.Contains(messages, expected) {
				t.Errorf("missing compact result %q", expected)
			}
		}
		for _, private := range []string{"PRIVATE_CHILD_HISTORY", "GLOBAL_SKILL_POLICY", "context_snapshot"} {
			if strings.Contains(messages, private) {
				t.Errorf("leaked private runtime context %q", private)
			}
		}
		return llm.Turn{Content: "调查完成，来源：https://example.com/source", Usage: llm.Usage{PromptTokens: 7, CompletionTokens: 3}}, nil
	}}
	core := agent.NewAgent(1, "recall", model, "GLOBAL_SKILL_POLICY", 6, agent.NewAbility("dangerous", "dangerous", "GLOBAL_SKILL_POLICY", agent.ToolBox{{Schema: llm.ToolSchema{Name: "save_artifact"}}}))
	report, err := NewChatService(core, nil, nil).GenerateReport(context.Background(), autoReportFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || report.Message.Role != "assistant" || !strings.Contains(report.Message.Content, "调查完成") || report.Error != "" {
		t.Fatalf("unexpected report: %+v calls=%d", report, model.calls)
	}
	if report.Run == nil || report.Run.RunID == "child-run" || report.Run.SubagentID != "" || report.Run.ParentRunID != "" || report.Run.ConversationID != 7 || report.Run.PromptTokens != 7 {
		t.Fatalf("child consumption or identity leaked: %+v", report.Run)
	}
	var state runtime.RunState
	if err := json.Unmarshal(report.Run.State, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Calls) != 1 || len(state.Children) != 0 || len(state.Steps) != 0 || state.ContextSnapshot != nil {
		t.Fatalf("not an isolated rendering Run: %+v", state)
	}
	if len(state.InheritedSources) != 1 || state.InheritedSources[0].SourceRunID != "child-run" {
		t.Fatalf("report discarded source provenance: %+v", state.InheritedSources)
	}
	if len(report.Sources) != 1 || report.Sources[0] != state.InheritedSources[0] {
		t.Fatalf("report delivery discarded source metadata: %+v", report.Sources)
	}
}

func TestGenerateReportDefaultsToOriginalResultWithoutModelOrHistory(t *testing.T) {
	task := autoReportFixture(t)
	task.DeliveryMode = ""
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		t.Fatal("direct delivery called model")
		return llm.Turn{}, nil
	}}
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	database, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	service := NewChatService(agent.NewAgent(1, "recall", model, "", 6), memory.NewMemoryManger(database, nil), nil)
	report, err := service.GenerateReport(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if report.Message.Content != "KNOWN_FINDING" || report.Message.Role != "assistant" || report.Run != nil || report.Error != "" || model.calls != 0 {
		t.Fatalf("direct delivery rewrote result or consumed model: %+v calls=%d", report, model.calls)
	}
	if len(report.Sources) != 1 || report.Sources[0].SourceRunID != "child-run" || report.Sources[0].URL != "https://example.com/source" {
		t.Fatalf("direct source provenance lost: %+v", report.Sources)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateReportDirectPartialWorksWithoutConfiguredModel(t *testing.T) {
	task := autoReportFixture(t)
	task.DeliveryMode, task.Status, task.Error = delivery.Direct, "partial", "second source unavailable"
	report, err := NewChatService(nil, nil, nil).GenerateReport(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if report.Run != nil || report.Error != "" || len(report.Sources) != 1 || !strings.Contains(report.Message.Content, "仅完成部分调查") || !strings.Contains(report.Message.Content, "KNOWN_FINDING") || !strings.Contains(report.Message.Content, task.Error) {
		t.Fatalf("partial delivery lost result, error or sources: %+v", report)
	}
}

func TestGenerateReportDirectRejectsOversizedOriginalInsteadOfTruncating(t *testing.T) {
	task := autoReportFixture(t)
	task.DeliveryMode = ""
	var child runtime.RunResult
	if err := json.Unmarshal(task.Result, &child); err != nil {
		t.Fatal(err)
	}
	child.Content = strings.Repeat("x", (512<<10)+1)
	task.Result, _ = json.Marshal(child)
	if _, err := NewChatService(nil, nil, nil).GenerateReport(context.Background(), task); err == nil || !strings.Contains(err.Error(), "storage limit") {
		t.Fatalf("oversized original silently lost: %v", err)
	}
}

func TestGenerateReportRejectsUnknownDeliveryModeEvenForFailedTask(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		task := autoReportFixture(t)
		task.Status, task.DeliveryMode = status, delivery.Combined
		if _, err := NewChatService(nil, nil, nil).GenerateReport(context.Background(), task); err == nil {
			t.Fatal("unsupported single-task delivery mode accepted")
		}
	}
}

func TestGenerateReportFailureKeepsFailedRunAndDoesNotRetryModel(t *testing.T) {
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Usage: llm.Usage{PromptTokens: 11}}, errors.New("provider timed out")
	}}
	report, err := NewChatService(agent.NewAgent(1, "recall", model, "", 6), nil, nil).GenerateReport(context.Background(), autoReportFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || report.Run == nil || report.Run.Status != "failed" || report.Run.PromptTokens != 11 || len(report.Sources) != 1 || !strings.Contains(report.Message.Content, "自动整理失败") || !strings.Contains(report.Error, "provider timed out") {
		t.Fatalf("failure lost or retried: %+v calls=%d", report, model.calls)
	}
}

func TestGenerateReportRejectsUnsolicitedToolCallsWithoutExecution(t *testing.T) {
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "write", Name: "save_artifact", Arguments: `{}`}}, Usage: llm.Usage{PromptTokens: 5}}, nil
	}}
	var writes int
	core := agent.NewAgent(1, "recall", model, "", 6, agent.NewAbility("write", "write", "", agent.ToolBox{{Schema: llm.ToolSchema{Name: "save_artifact"}, Run: func(context.Context, string) (string, error) { writes++; return "saved", nil }}}))
	report, err := NewChatService(core, nil, nil).GenerateReport(context.Background(), autoReportFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 || model.calls != 1 || !strings.Contains(report.Error, "cannot execute tools") || report.Run.PromptTokens != 5 {
		t.Fatalf("tool call escaped rendering: %+v writes=%d", report, writes)
	}
}

func TestGenerateReportTerminalFailureAndMissingModelHaveDeterministicMessages(t *testing.T) {
	model := &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		panic("unexpected model call")
	}}
	service := NewChatService(agent.NewAgent(1, "recall", model, "", 6), nil, nil)
	for _, status := range []string{"failed", "cancelled", "interrupted"} {
		task := autoReportFixture(t)
		task.Status = status
		task.Error = "original failure"
		report, err := service.GenerateReport(context.Background(), task)
		label := map[string]string{"failed": "执行失败", "cancelled": "已取消", "interrupted": "执行中断"}[status]
		if err != nil || report.Run != nil || !strings.Contains(report.Message.Content, label) || strings.Contains(report.Message.Content, status) || strings.Contains(report.Message.Content, task.ID) || !strings.Contains(report.Message.Content, "original failure") {
			t.Fatalf("bad deterministic %s: %+v %v", status, report, err)
		}
	}
	report, err := NewChatService(nil, nil, nil).GenerateReport(context.Background(), autoReportFixture(t))
	if err != nil || !strings.Contains(report.Error, "not configured") || report.Run != nil || len(report.Sources) != 1 {
		t.Fatalf("missing model not explicit: %+v %v", report, err)
	}
	if !strings.Contains(report.Message.Content, "结果已保存") || strings.Contains(report.Message.Content, "task-id") {
		t.Fatalf("fallback exposed implementation identifiers: %s", report.Message.Content)
	}
}

func TestGenerateReportRejectsForeignOrUnfinishedResultsBeforeModel(t *testing.T) {
	service := NewChatService(agent.NewAgent(1, "recall", &reportTestModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		panic("unexpected model call")
	}}, "", 6), nil, nil)
	for _, mutate := range []func(*types.SubagentTask){func(task *types.SubagentTask) { task.ConversationID = 8 }, func(task *types.SubagentTask) { task.SubagentID = "other" }, func(task *types.SubagentTask) { task.RunID = "other" }, func(task *types.SubagentTask) { task.Status = "running" }, func(task *types.SubagentTask) { task.Result = json.RawMessage(`null`) }} {
		task := autoReportFixture(t)
		mutate(&task)
		if _, err := service.GenerateReport(context.Background(), task); err == nil {
			t.Fatal("invalid result reached model")
		}
	}
}

func TestGenerateReportLoadsOnlyCurrentConversationRecentHistory(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	database, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE conversation_id = .* AND source_tag = .* ORDER BY .* LIMIT`).WithArgs(uint(7), types.SourceChat, 1).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(11, 7, types.SourceChat))
	rows := sqlmock.NewRows([]string{"session_id", "seq", "role", "content"})
	for i := 1; i <= 25; i++ {
		rows.AddRow(11, i, "user", fmt.Sprintf("OWN_MESSAGE_%02d", i))
	}
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .* ORDER BY seq`).WithArgs(uint(11)).WillReturnRows(rows)
	model := &reportTestModel{next: func(_ context.Context, conversation *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		snapshot, err := conversation.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		messages := string(snapshot.Messages)
		if strings.Contains(messages, "OWN_MESSAGE_05") || !strings.Contains(messages, "OWN_MESSAGE_06") || !strings.Contains(messages, "OWN_MESSAGE_25") {
			t.Fatalf("history limit not respected: %s", messages)
		}
		return llm.Turn{Content: "report"}, nil
	}}
	service := NewChatService(agent.NewAgent(1, "recall", model, "", 6), memory.NewMemoryManger(database, nil), nil)
	if _, err := service.GenerateReport(context.Background(), autoReportFixture(t)); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReportHistoryBudgetPreservesValidUTF8(t *testing.T) {
	value := strings.Repeat("回报", 40<<10)
	clipped := clipReportText(value, 64<<10)
	if len(clipped) > 64<<10 || !json.Valid([]byte(fmt.Sprintf("%q", clipped))) {
		t.Fatal("history budget or UTF8 broken")
	}
}

func TestChatHoldsReportGateDuringUserModelCall(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	model := &reportTestModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		close(entered)
		select {
		case <-release:
			return llm.Turn{Content: "user response"}, nil
		case <-ctx.Done():
			return llm.Turn{}, ctx.Err()
		}
	}}
	service := NewChatService(agent.NewAgent(1, "recall", model, "", 6), nil, nil)
	done := make(chan error, 1)
	go func() { _, err := service.Chat(context.Background(), "ordinary question", 7); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Chat did not enter model")
	}
	if unlock, ok := service.TryReportLock(7); ok {
		unlock()
		t.Fatal("report interrupted current Chat")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	unlock, ok := service.TryReportLock(7)
	if !ok {
		t.Fatal("Chat did not release report gate")
	}
	unlock()
}

type reportTestTurnWriter struct {
	sessionID        uint
	question, answer string
	calls            int
	err              error
}

func (w *reportTestTurnWriter) AppendTurn(_ context.Context, sessionID uint, question, answer string) error {
	w.calls++
	w.sessionID = sessionID
	w.question = question
	w.answer = answer
	return w.err
}

func TestChatTurnWriterReceivesTurnAndPropagatesPersistenceFailure(t *testing.T) {
	writer := &reportTestTurnWriter{err: errors.New("event transaction failed")}
	service := NewChatService(nil, nil, nil).WithTurnWriter(writer)
	err := service.saveTurn(context.Background(), 11, "question", "answer")
	if !errors.Is(err, writer.err) || writer.calls != 1 || writer.sessionID != 11 || writer.question != "question" || writer.answer != "answer" {
		t.Fatalf("turn writer not used: %+v %v", writer, err)
	}
	if err := service.saveTurn(context.Background(), 0, "ignored", "ignored"); err != nil || writer.calls != 1 {
		t.Fatal("unpersisted test turn unexpectedly emitted events")
	}
}
