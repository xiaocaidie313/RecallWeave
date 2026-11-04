package chat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/chat"
	"recallweave/internal/llm"
	"recallweave/internal/types"
)

func TestSlowGroupSynthesisDoesNotBlockDirectDeliveryBehindManySynthesisTasks(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	model := &dispatchModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return llm.Turn{Content: "summary"}, nil
		case <-ctx.Done():
			return llm.Turn{}, ctx.Err()
		}
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	ledger, groupID, ids := newGroupLedger()
	second := ledger.rows[ids[1]]
	second.Status, second.Error = "failed", "LOOKUP_FAILED"
	ledger.rows[ids[1]] = second
	group := ledger.groups[groupID]
	group.Status, group.ReportStatus = "partial", "pending"
	ledger.groups[groupID] = group
	d, err := chat.StartReportDispatcher(context.Background(), service, ledger, chat.ReportConfig{PollInterval: time.Millisecond, Timeout: time.Second, DeliveryWorkers: 1, SynthesisWorkers: 1})
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
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("group synthesis did not start")
	}
	ledger.mu.Lock()
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("%032x", i+1)
		ledger.rows[id] = dispatchTask(uint(i+10), id)
	}
	directID := strings.Repeat("f", 32)
	direct := dispatchTask(2, directID)
	direct.DeliveryMode = "direct"
	ledger.rows[directID] = direct
	ledger.mu.Unlock()
	waitReport(t, &ledger.dispatchLedger, directID, "delivered")
	ledger.mu.Lock()
	if calls.Load() != 1 || ledger.groups[groupID].ReportStatus != "processing" || len(ledger.messages[2]) != 1 || ledger.rows[directID].ReportRunID != "" {
		ledger.mu.Unlock()
		t.Fatal("direct delivery waited for synthesis or caused another model call")
	}
	ledger.mu.Unlock()
	// Shutdown must cancel the still-running synthesis and wait for its worker.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSlowSynthesisDoesNotBlockCombinedGroupDelivery(t *testing.T) {
	entered := make(chan struct{})
	model := &dispatchModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		close(entered)
		<-ctx.Done()
		return llm.Turn{}, ctx.Err()
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	ledger, id, ids := newGroupLedger()
	group := ledger.groups[id]
	group.ConversationID = 2
	group.DeliveryMode = "combined"
	ledger.groups[id] = group
	for _, taskID := range ids {
		row := ledger.rows[taskID]
		row.ConversationID = 2
		if len(row.Result) > 0 {
			var result runtime.RunResult
			if err := json.Unmarshal(row.Result, &result); err != nil {
				t.Fatal(err)
			}
			result.State.ConversationID = 2
			row.Result, _ = json.Marshal(result)
		}
		ledger.rows[taskID] = row
	}
	slowID := strings.Repeat("e", 32)
	ledger.rows[slowID] = dispatchTask(1, slowID)
	startGroupDispatcher(t, service, ledger)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("synthesis did not start")
	}
	ledger.mu.Lock()
	second := ledger.rows[ids[1]]
	second.Status, second.Error = "failed", "LOOKUP_FAILED"
	ledger.rows[ids[1]] = second
	group.Status, group.ReportStatus = "partial", "pending"
	ledger.groups[id] = group
	ledger.mu.Unlock()
	completed := waitGroupReport(t, ledger, id, "delivered")
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if completed.ReportRunID != "" || len(ledger.messages[2]) != 1 || !strings.Contains(ledger.messages[2][0].Content, "VERIFIED_FINDINGS") || ledger.rows[slowID].ReportStatus != "processing" {
		t.Fatal("combined delivery was blocked by another conversation's model")
	}
}

func TestConcurrentReportLanesStillSerializeTheSameConversation(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	model := &dispatchModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		return llm.Turn{}, ctx.Err()
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	firstID, secondID, directID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{firstID: dispatchTask(1, firstID), secondID: dispatchTask(1, secondID)}}
	startDispatcher(t, service, ledger)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first report did not start")
	}
	ledger.mu.Lock()
	direct := dispatchTask(1, directID)
	direct.DeliveryMode = "direct"
	ledger.rows[directID] = direct
	otherID := strings.Repeat("f", 32)
	other := dispatchTask(2, otherID)
	other.DeliveryMode = "direct"
	ledger.rows[otherID] = other
	ledger.mu.Unlock()
	waitReport(t, ledger, otherID, "delivered")
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if calls.Load() != 1 || ledger.rows[secondID].ReportStatus != "pending" || ledger.rows[directID].ReportStatus != "pending" {
		t.Fatal("another lane bypassed the active conversation's report lock")
	}
}

func TestBusyConversationDoesNotHideDirectDeliveryBehindFirstPage(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	model := &dispatchModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		close(entered)
		select {
		case <-release:
			return llm.Turn{Content: "user answer"}, nil
		case <-ctx.Done():
			return llm.Turn{}, ctx.Err()
		}
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	ledger := &dispatchLedger{rows: make(map[string]types.SubagentTask)}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("%032x", i+1)
		row := dispatchTask(1, id)
		row.DeliveryMode = "direct"
		ledger.rows[id] = row
	}
	id := strings.Repeat("f", 32)
	row := dispatchTask(2, id)
	row.DeliveryMode = "direct"
	ledger.rows[id] = row
	userDone := make(chan error, 1)
	go func() { _, err := service.Chat(context.Background(), "busy user turn", 1); userDone <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("user turn did not start")
	}
	startDispatcher(t, service, ledger)
	waitReport(t, ledger, id, "delivered")
	ledger.mu.Lock()
	for _, row := range ledger.rows {
		if row.ConversationID == 1 && row.ReportStatus != "pending" {
			ledger.mu.Unlock()
			t.Fatal("report preempted active user")
		}
	}
	ledger.mu.Unlock()
	once.Do(func() { close(release) })
	if err := <-userDone; err != nil {
		t.Fatal(err)
	}
}

func TestReportDispatcherBoundsSynthesisAndWaitsForWorkerCleanup(t *testing.T) {
	entered := make(chan struct{}, 10)
	cancelled, cleanupRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(cleanupRelease) })
	var active, maxActive atomic.Int32
	var cancelledOnce sync.Once
	model := &dispatchModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for previous := maxActive.Load(); count > previous; previous = maxActive.Load() {
			if maxActive.CompareAndSwap(previous, count) {
				break
			}
		}
		entered <- struct{}{}
		<-ctx.Done()
		cancelledOnce.Do(func() { close(cancelled) })
		<-cleanupRelease
		return llm.Turn{}, ctx.Err()
	}}
	service := chat.NewChatService(agent.NewAgent(1, "recall", model, "", 2), nil, nil)
	ledger := &dispatchLedger{rows: make(map[string]types.SubagentTask)}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("%032x", i+1)
		ledger.rows[id] = dispatchTask(uint(i+1), id)
	}
	d, err := chat.StartReportDispatcher(context.Background(), service, ledger, chat.ReportConfig{PollInterval: time.Millisecond, Timeout: time.Second, DeliveryWorkers: 1, SynthesisWorkers: 2})
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
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("synthesis worker limit exceeded")
	case <-time.After(20 * time.Millisecond):
	}
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		closeDone <- d.Close(ctx)
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel workers")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before workers completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(cleanupRelease) })
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 || maxActive.Load() != 2 {
		t.Fatalf("worker lifecycle incorrect: active=%d maximum=%d", active.Load(), maxActive.Load())
	}
	unlock, ok := service.TryReportLock(1)
	if !ok {
		t.Fatal("shutdown leaked the conversation lock")
	}
	unlock()
}

type cancelledDeliveryClaim struct {
	*dispatchLedger
	entered chan struct{}
}

func (l *cancelledDeliveryClaim) Claim(ctx context.Context, id string, owner uint, claim string) (types.SubagentTask, bool, error) {
	row, claimed, err := l.dispatchLedger.Claim(ctx, id, owner, claim)
	if err != nil || !claimed {
		return row, claimed, err
	}
	close(l.entered)
	<-ctx.Done()
	return types.SubagentTask{}, false, ctx.Err()
}

func TestDirectDeliveryClaimInterruptedByShutdownResumesWithoutModel(t *testing.T) {
	service, calls := noDeliveryModel(t)
	id := strings.Repeat("a", 32)
	task := dispatchTask(1, id)
	task.DeliveryMode = "direct"
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id: task}}
	wrapper := &cancelledDeliveryClaim{dispatchLedger: ledger, entered: make(chan struct{})}
	d, err := chat.StartReportDispatcher(context.Background(), service, wrapper, chat.ReportConfig{PollInterval: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-wrapper.entered:
	case <-time.After(time.Second):
		t.Fatal("direct result was not claimed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	status := ledger.rows[id].ReportStatus
	ledger.mu.Unlock()
	if status != "processing" {
		t.Fatalf("interrupted deterministic result was terminally failed: %s", status)
	}
	startDispatcher(t, service, ledger)
	waitReport(t, ledger, id, "delivered")
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if calls.Load() != 0 || len(ledger.messages[1]) != 1 || len(ledger.events) != 1 || len(ledger.runs) != 0 {
		t.Fatal("recovery regenerated model work or duplicated the saved result")
	}
}
