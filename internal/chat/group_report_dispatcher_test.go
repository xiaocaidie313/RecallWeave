package chat_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/chat"
	"recallweave/internal/llm"
	"recallweave/internal/types"

	apphttp "recallweave/internal/http"

	"github.com/gin-gonic/gin"
)

type groupLedger struct {
	dispatchLedger
	groups            map[string]types.TaskGroup
	groupDeliverError error
}

func newGroupLedger() (*groupLedger, string, []string) {
	id := strings.Repeat("a", 32)
	ids := []string{strings.Repeat("b", 32), strings.Repeat("c", 32)}
	first, second := dispatchTask(1, ids[0]), dispatchTask(1, ids[1])
	first.GroupID, second.GroupID = id, id
	first.DeliveryMode, second.DeliveryMode = "direct", "direct"
	first.ParentRunID, second.ParentRunID = "parent", "parent"
	first.ReportStatus, second.ReportStatus = "grouped", "grouped"
	second.Status, second.Result, second.RunID = "running", nil, ""
	raw, _ := json.Marshal(ids)
	group := types.TaskGroup{ID: id, ConversationID: 1, ParentRunID: "parent", Goal: "GROUP_GOAL", ExpectedOutput: "Compare both findings and explicitly state failures", DeliveryMode: "synthesize", TaskIDs: raw, Status: "running"}
	return &groupLedger{dispatchLedger: dispatchLedger{rows: map[string]types.SubagentTask{ids[0]: first, ids[1]: second}}, groups: map[string]types.TaskGroup{id: group}}, id, ids
}
func (l *groupLedger) PendingGroups(ctx context.Context, limit int) ([]types.TaskGroup, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []types.TaskGroup
	for _, group := range l.groups {
		if group.ReportStatus == "pending" {
			rows = append(rows, types.TaskGroup{ID: group.ID, ConversationID: group.ConversationID, DeliveryMode: group.DeliveryMode})
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}
func (l *groupLedger) PendingGroupDelivery(ctx context.Context, limit int, synthesize bool, excluded []uint) ([]types.TaskGroup, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []types.TaskGroup
	for _, row := range l.groups {
		if row.ReportStatus != "pending" || row.DeliveryMode == "direct" || (row.DeliveryMode == "synthesize") != synthesize {
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
func (l *groupLedger) ClaimGroup(ctx context.Context, id string, owner uint, claim string) (types.TaskGroup, []types.SubagentTask, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return types.TaskGroup{}, nil, false, err
	}
	group, ok := l.groups[id]
	if !ok || group.ConversationID != owner {
		return group, nil, false, errors.New("not found")
	}
	if group.ReportStatus != "pending" {
		return group, nil, false, nil
	}
	group.ReportStatus, group.ReportClaim = "processing", claim
	l.groups[id] = group
	var ids []string
	_ = json.Unmarshal(group.TaskIDs, &ids)
	members := make([]types.SubagentTask, 0, len(ids))
	for _, taskID := range ids {
		members = append(members, l.rows[taskID])
	}
	return group, members, true, nil
}
func (l *groupLedger) DeliverGroup(ctx context.Context, id string, owner uint, claim string, report chat.GeneratedReport) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.groupDeliverError != nil {
		return l.groupDeliverError
	}
	group := l.groups[id]
	if group.ConversationID != owner {
		return errors.New("foreign owner")
	}
	if group.ReportStatus == "delivered" {
		return nil
	}
	if group.ReportStatus != "processing" || group.ReportClaim != claim {
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
	group.ReportStatus, group.ReportMessageID, group.ReportError = "delivered", message.ID, report.Error
	if report.Run != nil {
		l.runs[report.Run.RunID] = *report.Run
		group.ReportRunID = report.Run.RunID
	}
	now := time.Now()
	group.ReportedAt = &now
	l.groups[id] = group
	l.appendEvent(owner, "message.created", map[string]any{"group_id": id, "sources": report.Sources, "message": message})
	return nil
}
func (l *groupLedger) FailGroup(ctx context.Context, id string, owner uint, claim, reason string, run *types.AgentRun) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	group := l.groups[id]
	if group.ConversationID != owner {
		return errors.New("foreign owner")
	}
	if group.ReportStatus == "failed" || group.ReportStatus == "delivered" {
		return nil
	}
	if group.ReportClaim != claim {
		return errors.New("claim mismatch")
	}
	group.ReportStatus, group.ReportError = "failed", reason
	l.groups[id] = group
	if run != nil {
		if l.runs == nil {
			l.runs = make(map[string]types.AgentRun)
		}
		l.runs[run.RunID] = *run
	}
	l.appendEvent(owner, "report.failed", map[string]string{"group_id": id})
	return nil
}
func (l *groupLedger) InterruptGroupProcessing(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, group := range l.groups {
		if group.ReportStatus == "processing" {
			if group.DeliveryMode == "synthesize" {
				group.ReportStatus = "failed"
				group.ReportError = "interrupted without model replay"
			} else {
				group.ReportStatus, group.ReportClaim, group.ReportError = "pending", "", ""
			}
			l.groups[id] = group
		}
	}
	return nil
}
func startGroupDispatcher(t *testing.T, service *chat.ChatService, ledger *groupLedger) *chat.ReportDispatcher {
	t.Helper()
	dispatcher, err := chat.StartReportDispatcher(context.Background(), service, ledger, chat.ReportConfig{PollInterval: 5 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := dispatcher.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return dispatcher
}
func waitGroupReport(t *testing.T, ledger *groupLedger, id, status string) types.TaskGroup {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		group := ledger.groups[id]
		ledger.mu.Unlock()
		if group.ReportStatus == status {
			return group
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("group report did not become %s", status)
	return types.TaskGroup{}
}

func TestGroupDispatcherWaitsForLastMemberAndSendsOneCombinedSSEMessage(t *testing.T) {
	var calls atomic.Int32
	model := &dispatchModel{next: func(_ context.Context, conv *llm.Conversation, schemas []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		snapshot, err := conv.Snapshot()
		if err != nil {
			return llm.Turn{}, err
		}
		body := string(snapshot.Messages)
		if len(schemas) != 0 || !strings.Contains(body, "GROUP_GOAL") || !strings.Contains(body, "VERIFIED_FINDINGS") || !strings.Contains(body, "LOOKUP_FAILED") {
			return llm.Turn{}, errors.New("combined report lost a result, error, or goal")
		}
		return llm.Turn{Content: "COMBINED_REPORT: source A is available; source B failed", Usage: llm.Usage{PromptTokens: 13}}, nil
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil, nil)
	ledger, id, ids := newGroupLedger()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	apphttp.MountConversationEvents(router, apphttp.NewEventsHandler(ledger))
	server := httptest.NewServer(router)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	stream, err := client.Get(server.URL + "/api/conversations/1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	startGroupDispatcher(t, service, ledger)
	time.Sleep(25 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("first child triggered an early report")
	}
	ledger.mu.Lock()
	second := ledger.rows[ids[1]]
	second.Status, second.Error = "failed", "LOOKUP_FAILED"
	ledger.rows[ids[1]] = second
	group := ledger.groups[id]
	group.Status, group.ReportStatus = "partial", "pending"
	ledger.groups[id] = group
	ledger.appendEvent(1, "task_group.finished", map[string]string{"group_id": id})
	ledger.mu.Unlock()
	completed := waitGroupReport(t, ledger, id, "delivered")
	scanner := bufio.NewScanner(stream.Body)
	received := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "COMBINED_REPORT") {
			if !strings.Contains(line, id) {
				t.Fatal("SSE report missing group identity")
			}
			received = true
			break
		}
	}
	if !received {
		t.Fatalf("combined report not pushed: %v", scanner.Err())
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	run := ledger.runs[completed.ReportRunID]
	if calls.Load() != 1 || len(ledger.messages[1]) != 1 || run.PromptTokens != 13 || run.SubagentID != "" {
		t.Fatalf("duplicate reports or rebilled children: calls=%d messages=%d run=%+v", calls.Load(), len(ledger.messages[1]), run)
	}
	if ledger.rows[ids[0]].ReportStatus != "grouped" || ledger.rows[ids[1]].ReportStatus != "grouped" {
		t.Fatal("members were individually reported")
	}
}

func TestGroupDispatcherAllCancelledReportsWithoutCallingModel(t *testing.T) {
	var calls atomic.Int32
	model := &dispatchModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		return llm.Turn{Content: "unexpected"}, nil
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil, nil)
	ledger, id, ids := newGroupLedger()
	ledger.mu.Lock()
	for _, taskID := range ids {
		member := ledger.rows[taskID]
		member.Status, member.Result, member.RunID, member.CancelRequested = "cancelled", nil, "", true
		ledger.rows[taskID] = member
	}
	group := ledger.groups[id]
	group.Status, group.CancelRequested, group.ReportStatus = "cancelled", true, "pending"
	ledger.groups[id] = group
	ledger.mu.Unlock()
	startGroupDispatcher(t, service, ledger)
	waitGroupReport(t, ledger, id, "delivered")
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if calls.Load() != 0 || len(ledger.messages[1]) != 1 || !strings.Contains(ledger.messages[1][0].Content, "已取消") {
		t.Fatalf("cancelled group generated model work or duplicate output: calls=%d messages=%+v", calls.Load(), ledger.messages[1])
	}
}

func TestGroupDispatcherDoesNotRegenerateAfterFailedPersistence(t *testing.T) {
	var calls atomic.Int32
	model := &dispatchModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		return llm.Turn{Content: "combined"}, nil
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil, nil)
	ledger, id, ids := newGroupLedger()
	ledger.mu.Lock()
	member := ledger.rows[ids[1]]
	member.Status, member.Error = "failed", "LOOKUP_FAILED"
	ledger.rows[ids[1]] = member
	group := ledger.groups[id]
	group.Status, group.ReportStatus = "partial", "pending"
	ledger.groups[id] = group
	ledger.groupDeliverError = errors.New("disk unavailable")
	ledger.mu.Unlock()
	dispatcher := startGroupDispatcher(t, service, ledger)
	waitGroupReport(t, ledger, id, "failed")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	ledger.groupDeliverError = nil
	ledger.mu.Unlock()
	startGroupDispatcher(t, service, ledger)
	time.Sleep(25 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("group model automatically repeated after persistence failure: %d", calls.Load())
	}
}
