package chat_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/chat"
	"recallweave/internal/events"
	apphttp "recallweave/internal/http"
	"recallweave/internal/llm"
	"recallweave/internal/types"
	"recallweave/internal/watch/useage"

	"github.com/gin-gonic/gin"
)

type dispatchLedger struct {
	mu           sync.Mutex
	rows         map[string]types.SubagentTask
	messages     map[uint][]types.Message
	runs         map[string]types.AgentRun
	events       []types.ConversationEvent
	deliverError error
	claimError   error
}

func dispatchTask(owner uint, id string) types.SubagentTask {
	child := runtime.RunResult{Content: "VERIFIED_FINDINGS", State: runtime.RunState{RunID: strings.Repeat("c", 32), SubagentID: strings.Repeat("d", 32), ConversationID: owner, AgentName: "researcher", Status: runtime.RunCompleted, Calls: []useage.ModelCallStats{{Usage: useage.TokenUsage{PromptTokens: 100}}}, InheritedSources: []runtime.Source{{Kind: "web", Tool: "mcp_tavily_search", URL: "https://example.test/source"}}}}
	raw, _ := json.Marshal(child)
	return types.SubagentTask{ID: id, SubagentID: child.State.SubagentID, RunID: child.State.RunID, ConversationID: owner, Task: "research", AgentName: "researcher", DeliveryMode: "synthesize", Status: "completed", Result: raw, ReportStatus: "pending"}
}

func (l *dispatchLedger) Pending(ctx context.Context, limit int) ([]types.SubagentTask, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []types.SubagentTask
	for _, row := range l.rows {
		if row.ReportStatus == "pending" {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
func (l *dispatchLedger) PendingDelivery(ctx context.Context, limit int, synthesize bool, excluded []uint) ([]types.SubagentTask, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []types.SubagentTask
	for _, row := range l.rows {
		if row.ReportStatus != "pending" || (row.DeliveryMode == "synthesize") != synthesize {
			continue
		}
		blocked := false
		for _, owner := range excluded {
			if owner == row.ConversationID {
				blocked = true
				break
			}
		}
		if !blocked {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
func (l *dispatchLedger) Claim(ctx context.Context, id string, owner uint, claim string) (types.SubagentTask, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return types.SubagentTask{}, false, err
	}
	row, ok := l.rows[id]
	if !ok || row.ConversationID != owner {
		return types.SubagentTask{}, false, errors.New("not found")
	}
	if row.ReportStatus != "pending" {
		return row, false, nil
	}
	row.ReportStatus, row.ReportClaim = "processing", claim
	l.rows[id] = row
	if l.claimError != nil {
		return types.SubagentTask{}, false, l.claimError
	}
	return row, true, nil
}
func (l *dispatchLedger) appendEvent(owner uint, kind string, data any) {
	raw, _ := json.Marshal(data)
	l.events = append(l.events, types.ConversationEvent{ID: uint64(len(l.events) + 1), ConversationID: owner, Kind: kind, Data: raw})
}
func (l *dispatchLedger) Deliver(ctx context.Context, id string, owner uint, claim string, report chat.GeneratedReport) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.deliverError != nil {
		return l.deliverError
	}
	row := l.rows[id]
	if row.ConversationID != owner {
		return errors.New("foreign owner")
	}
	if row.ReportStatus == "delivered" {
		return nil
	}
	if row.ReportStatus != "processing" || row.ReportClaim != claim {
		return errors.New("claim mismatch")
	}
	if l.messages == nil {
		l.messages = make(map[uint][]types.Message)
	}
	if l.runs == nil {
		l.runs = make(map[string]types.AgentRun)
	}
	message := report.Message
	message.ID = uint(len(l.events) + 100)
	message.SessionID = owner + 100
	message.Seq = len(l.messages[owner]) + 1
	l.messages[owner] = append(l.messages[owner], message)
	row.ReportStatus, row.ReportMessageID, row.ReportError = "delivered", message.ID, report.Error
	if report.Run != nil {
		l.runs[report.Run.RunID] = *report.Run
		row.ReportRunID = report.Run.RunID
	}
	now := time.Now()
	row.ReportedAt = &now
	l.rows[id] = row
	l.appendEvent(owner, "message.created", map[string]any{"task_id": id, "group_id": row.GroupID, "task_run_id": row.RunID, "sources": report.Sources, "message": message})
	return nil
}
func (l *dispatchLedger) Fail(ctx context.Context, id string, owner uint, claim, reason string, run *types.AgentRun) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	row := l.rows[id]
	if row.ConversationID != owner {
		return errors.New("foreign owner")
	}
	if row.ReportStatus == "failed" || row.ReportStatus == "delivered" {
		return nil
	}
	if row.ReportClaim != claim {
		return errors.New("claim mismatch")
	}
	row.ReportStatus, row.ReportError = "failed", reason
	l.rows[id] = row
	if run != nil {
		if l.runs == nil {
			l.runs = make(map[string]types.AgentRun)
		}
		l.runs[run.RunID] = *run
	}
	l.appendEvent(owner, "report.failed", map[string]any{"task_id": id})
	return nil
}
func (l *dispatchLedger) InterruptProcessing(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, row := range l.rows {
		if row.ReportStatus == "processing" {
			if row.DeliveryMode == "synthesize" {
				row.ReportStatus = "failed"
				row.ReportError = "report interrupted; not replayed"
			} else {
				row.ReportStatus, row.ReportClaim, row.ReportError = "pending", "", ""
			}
			l.rows[id] = row
		}
	}
	return nil
}
func (l *dispatchLedger) ConversationExists(_ context.Context, owner uint) (bool, error) {
	return owner == 1 || owner == 2, nil
}
func (l *dispatchLedger) ValidateCursor(_ context.Context, owner uint, after uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if after == 0 {
		return nil
	}
	for _, event := range l.events {
		if event.ID == after && event.ConversationID == owner {
			return nil
		}
	}
	return events.ErrInvalidCursor
}
func (l *dispatchLedger) List(ctx context.Context, owner uint, after uint64, limit int) ([]types.ConversationEvent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []types.ConversationEvent
	for _, event := range l.events {
		if event.ConversationID == owner && event.ID > after {
			rows = append(rows, event)
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}

type dispatchModel struct {
	next func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error)
}

func (m *dispatchModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected auxiliary model call")
}
func (m *dispatchModel) Next(ctx context.Context, conv *llm.Conversation, schemas []llm.ToolSchema) (llm.Turn, error) {
	turn, err := m.next(ctx, conv, schemas)
	if err == nil {
		conv.AddAssistantTurn(turn)
	}
	return turn, err
}

func startDispatcher(t *testing.T, service *chat.Agent, ledger *dispatchLedger) *chat.ReportDispatcher {
	t.Helper()
	d, err := chat.StartReportDispatcher(context.Background(), service, ledger, chat.ReportConfig{PollInterval: 5 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return d
}
func waitReport(t *testing.T, ledger *dispatchLedger, id, status string) types.SubagentTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		row := ledger.rows[id]
		ledger.mu.Unlock()
		if row.ReportStatus == status {
			return row
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("report %s did not become %s", id, status)
	return types.SubagentTask{}
}

func TestReportDispatcherWaitsForUserAndDoesNotBlockOtherConversation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var reportCalls atomic.Int32
	model := &dispatchModel{next: func(ctx context.Context, conv *llm.Conversation, schemas []llm.ToolSchema) (llm.Turn, error) {
		snapshot, err := conv.Snapshot()
		if err != nil {
			return llm.Turn{}, err
		}
		if strings.Contains(string(snapshot.Messages), "BLOCK_USER") {
			close(entered)
			select {
			case <-release:
				return llm.Turn{Content: "user answer"}, nil
			case <-ctx.Done():
				return llm.Turn{}, ctx.Err()
			}
		}
		if len(schemas) != 0 {
			return llm.Turn{}, errors.New("report received tools")
		}
		reportCalls.Add(1)
		return llm.Turn{Content: "reported findings", Usage: llm.Usage{PromptTokens: 9}}, nil
	}}
	service := chat.NewAgent(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	id1, id2 := strings.Repeat("a", 32), strings.Repeat("b", 32)
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id1: dispatchTask(1, id1), id2: dispatchTask(2, id2)}}
	userDone := make(chan error, 1)
	go func() { _, err := service.Chat(context.Background(), "BLOCK_USER", 1); userDone <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("user turn not started")
	}
	startDispatcher(t, service, ledger)
	waitReport(t, ledger, id2, "delivered")
	ledger.mu.Lock()
	waiting := ledger.rows[id1].ReportStatus
	ledger.mu.Unlock()
	if waiting != "pending" {
		t.Fatalf("report interrupted user: %s", waiting)
	}
	close(release)
	if err := <-userDone; err != nil {
		t.Fatal(err)
	}
	row := waitReport(t, ledger, id1, "delivered")
	ledger.mu.Lock()
	run := ledger.runs[row.ReportRunID]
	count := len(ledger.messages[1])
	ledger.mu.Unlock()
	if reportCalls.Load() != 2 || count != 1 || run.PromptTokens != 9 || run.SubagentID != "" {
		t.Fatalf("wrong report calls/accounting: calls=%d messages=%d run=%+v", reportCalls.Load(), count, run)
	}
}

func TestReportDispatcherDoesNotRetryModelAfterDeliveryFailureOrRestart(t *testing.T) {
	var calls atomic.Int32
	model := &dispatchModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		return llm.Turn{Content: "result"}, nil
	}}
	service := chat.NewAgent(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	id := strings.Repeat("a", 32)
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id: dispatchTask(1, id)}, deliverError: errors.New("disk unavailable")}
	d := startDispatcher(t, service, ledger)
	waitReport(t, ledger, id, "failed")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	ledger.deliverError = nil
	ledger.mu.Unlock()
	startDispatcher(t, service, ledger)
	time.Sleep(25 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("model repeated after failed delivery: %d", calls.Load())
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.messages[1]) != 0 || len(ledger.events) != 1 || ledger.events[0].Kind != "report.failed" {
		t.Fatalf("failure visibility incorrect: %+v", ledger.events)
	}
}

func TestReportDispatcherAndSSEDeliverOnceAcrossTwoTabsAndReplay(t *testing.T) {
	var calls atomic.Int32
	model := &dispatchModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		return llm.Turn{Content: "AUTOMATIC_REPORT"}, nil
	}}
	service := chat.NewAgent(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	id := strings.Repeat("a", 32)
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id: dispatchTask(1, id)}}
	ledger.mu.Lock()
	ledger.appendEvent(1, "task.finished", map[string]string{"task_id": id})
	ledger.mu.Unlock()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	apphttp.MountConversationEvents(router, apphttp.NewEventsHandler(ledger))
	server := httptest.NewServer(router)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	first, err := client.Get(server.URL + "/api/conversations/1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	second, err := client.Get(server.URL + "/api/conversations/1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if first.StatusCode != http.StatusOK || !strings.HasPrefix(first.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("bad SSE handshake: %v", first.Status)
	}
	startDispatcher(t, service, ledger)
	readMessage := func(response *http.Response) (uint64, string) {
		t.Helper()
		scanner := bufio.NewScanner(response.Body)
		var eventID uint64
		var kind, data string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "id:") {
				eventID, _ = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64)
			}
			if strings.HasPrefix(line, "event:") {
				kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
			if line == "" && kind == "message.created" {
				return eventID, data
			}
		}
		t.Fatalf("SSE report not received: %v", scanner.Err())
		return 0, ""
	}
	id1, data1 := readMessage(first)
	id2, data2 := readMessage(second)
	if id1 != id2 || !strings.Contains(data1, "AUTOMATIC_REPORT") || data1 != data2 {
		t.Fatalf("tabs got different saved messages: %d %d %s %s", id1, id2, data1, data2)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/conversations/1/events?after=0", nil)
	request.Header.Set("Last-Event-ID", fmt.Sprint(id1-1))
	replayed, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.Body.Close()
	replayID, replayData := readMessage(replayed)
	if replayID != id1 || replayData != data1 || calls.Load() != 1 {
		t.Fatalf("replay regenerated message/model: id=%d calls=%d", replayID, calls.Load())
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.messages[1]) != 1 {
		t.Fatalf("duplicate report messages: %+v", ledger.messages[1])
	}
}

func TestReportDispatcherShutdownRecordsInterruptedGenerationWithoutRetry(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	model := &dispatchModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		return llm.Turn{}, ctx.Err()
	}}
	service := chat.NewAgent(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	id := strings.Repeat("a", 32)
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id: dispatchTask(1, id)}}
	d := startDispatcher(t, service, ledger)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("report generation did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	row := waitReport(t, ledger, id, "failed")
	if !strings.Contains(row.ReportError, "服务停止") {
		t.Fatalf("shutdown failure hidden: %+v", row)
	}
	startDispatcher(t, service, ledger)
	time.Sleep(25 * time.Millisecond)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if calls.Load() != 1 || len(ledger.messages[1]) != 0 || len(ledger.runs) != 1 {
		t.Fatalf("interrupted generation replayed or lost accounting: calls=%d messages=%d runs=%d", calls.Load(), len(ledger.messages[1]), len(ledger.runs))
	}
}

func TestReportDispatcherResolvesAmbiguousClaimWithoutCallingModel(t *testing.T) {
	var calls atomic.Int32
	model := &dispatchModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		return llm.Turn{Content: "unexpected"}, nil
	}}
	service := chat.NewAgent(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	id := strings.Repeat("a", 32)
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id: dispatchTask(1, id)}, claimError: errors.New("commit acknowledgement lost")}
	startDispatcher(t, service, ledger)
	row := waitReport(t, ledger, id, "failed")
	if calls.Load() != 0 || !strings.Contains(row.ReportError, "领取回报") {
		t.Fatalf("ambiguous claim executed a model or stayed stuck: calls=%d task=%+v", calls.Load(), row)
	}
}
