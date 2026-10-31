package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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

type memoryTaskStore struct {
	mu        sync.Mutex
	rows      map[string]store.SubagentTask
	runs      map[string]store.AgentRun
	createErr error
	finishErr error
}

func cloneTask(row store.SubagentTask) store.SubagentTask {
	row.Result = append(json.RawMessage(nil), row.Result...)
	return row
}

func (s *memoryTaskStore) Create(ctx context.Context, row *store.SubagentTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.createErr != nil {
		return s.createErr
	}
	if s.rows == nil {
		s.rows = make(map[string]store.SubagentTask)
	}
	if _, exists := s.rows[row.ID]; exists {
		return errors.New("duplicate")
	}
	s.rows[row.ID] = cloneTask(*row)
	return nil
}

func (s *memoryTaskStore) Load(ctx context.Context, id string, owner uint) (store.SubagentTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.SubagentTask{}, err
	}
	row, exists := s.rows[id]
	if !exists || owner == 0 || row.ConversationID != owner {
		return store.SubagentTask{}, ErrTaskNotFound
	}
	return cloneTask(row), nil
}

func (s *memoryTaskStore) List(ctx context.Context, owner uint, limit int) ([]store.SubagentTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []store.SubagentTask
	for _, row := range s.rows {
		if row.ConversationID == owner {
			rows = append(rows, cloneTask(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (s *memoryTaskStore) Start(ctx context.Context, id string, owner uint) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	row, exists := s.rows[id]
	if !exists || row.ConversationID != owner {
		return false, ErrTaskNotFound
	}
	if row.Status != "queued" || row.CancelRequested {
		return false, nil
	}
	now := time.Now()
	row.Status = "running"
	row.StartedAt = &now
	row.UpdatedAt = now
	s.rows[id] = row
	return true, nil
}

func (s *memoryTaskStore) Finish(ctx context.Context, row *store.SubagentTask, run *store.AgentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.finishErr != nil {
		return s.finishErr
	}
	old, exists := s.rows[row.ID]
	if !exists || old.ConversationID != row.ConversationID {
		return ErrTaskNotFound
	}
	if old.Status != "queued" && old.Status != "running" {
		return ErrTaskConflict
	}
	row.CancelRequested = old.CancelRequested
	if row.StartedAt == nil {
		row.StartedAt = old.StartedAt
	}
	s.rows[row.ID] = cloneTask(*row)
	if run != nil {
		if s.runs == nil {
			s.runs = make(map[string]store.AgentRun)
		}
		s.runs[run.RunID] = *run
	}
	return nil
}

func (s *memoryTaskStore) RequestCancel(ctx context.Context, id string, owner uint) (store.SubagentTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.SubagentTask{}, err
	}
	row, exists := s.rows[id]
	if !exists || row.ConversationID != owner {
		return store.SubagentTask{}, ErrTaskNotFound
	}
	if row.Status == "queued" || row.Status == "running" {
		row.CancelRequested = true
		row.UpdatedAt = time.Now()
		if row.Status == "queued" {
			row.Status = "cancelled"
			now := time.Now()
			row.FinishedAt = &now
		}
		s.rows[id] = row
	}
	return cloneTask(row), nil
}

func (s *memoryTaskStore) InterruptUnfinished(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, row := range s.rows {
		if row.Status == "queued" || row.Status == "running" {
			now := time.Now()
			row.Status = "interrupted"
			row.Error = "service restarted"
			row.FinishedAt = &now
			s.rows[id] = row
		}
	}
	return nil
}

func (s *memoryTaskStore) CheckTaskCheckpoint(ctx context.Context, session store.SubagentSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, row := range s.rows {
		if row.SubagentID == session.ID && row.ConversationID == session.ConversationID && row.PreviousRunID == session.LatestRunID && row.Status == "interrupted" && row.StartedAt != nil {
			return ErrCheckpointUnavailable
		}
	}
	return nil
}

func backgroundManager(t *testing.T, tasks *memoryTaskStore, sessions *continuationMemoryStore) *Manager {
	t.Helper()
	m := testManager(t, Config{}).WithSubagentSessions(sessions)
	if err := m.StartBackground(context.Background(), tasks, BackgroundConfig{Workers: 1, QueueCapacity: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.CloseBackground(ctx); err != nil {
			t.Error(err)
		}
	})
	return m
}

func waitBackground(t *testing.T, m *Manager, parent runtime.DelegationParent, id string, status string) runtime.BackgroundTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, err := m.GetTask(context.Background(), parent, id)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status == status {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %s did not become %s", id, status)
	return runtime.BackgroundTask{}
}

func TestBackgroundReturnsBeforeChildAndSurvivesRequestCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	model := &transcriptModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		close(entered)
		select {
		case <-release:
			return llm.Turn{Content: "saved findings", Usage: llm.Usage{PromptTokens: 17}}, nil
		case <-ctx.Done():
			return llm.Turn{}, ctx.Err()
		}
	}}
	tasks, sessions := &memoryTaskStore{}, &continuationMemoryStore{}
	m := backgroundManager(t, tasks, sessions)
	parent := managerParent(model)
	requestCtx, cancel := context.WithCancel(context.Background())
	handle, err := m.Submit(requestCtx, parent, runtime.DelegationRequest{Task: "investigate", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	if handle.Status != "queued" || handle.TaskID == "" || handle.SubagentID == "" || handle.Result != nil {
		t.Fatalf("bad receipt: %+v", handle)
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if _, err := m.GetTask(context.Background(), runtime.DelegationParent{ConversationID: parent.ConversationID + 1}, handle.TaskID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("foreign owner access: %v", err)
	}
	close(release)
	view := waitBackground(t, m, parent, handle.TaskID, "completed")
	if view.Result == nil || view.Result.Summary != "saved findings" || view.Result.Calls != 1 || view.Result.Usage.PromptTokens != 17 {
		t.Fatalf("bad result: %+v", view)
	}
	tasks.mu.Lock()
	persisted, ok := tasks.runs[view.RunID]
	tasks.mu.Unlock()
	if !ok || persisted.ParentRunID != parent.RunID || persisted.SubagentID != handle.SubagentID {
		t.Fatalf("missing independent Run: %+v", persisted)
	}
	if _, err := sessions.Load(context.Background(), handle.SubagentID, parent.ConversationID); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundBoundsQueueAndCancelsQueuedAndRunningWork(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	model := &transcriptModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		return llm.Turn{}, ctx.Err()
	}}
	tasks := &memoryTaskStore{}
	m := backgroundManager(t, tasks, &continuationMemoryStore{})
	parent := managerParent(model)
	first, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "first"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	second, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if rejected, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "third"}); !errors.Is(err, ErrBackgroundFull) || rejected.TaskID != "" {
		t.Fatalf("capacity not enforced: %+v %v", rejected, err)
	}
	view, err := m.CancelTask(context.Background(), parent, second.TaskID)
	if err != nil || view.Status != "cancelled" {
		t.Fatalf("queued cancel: %+v %v", view, err)
	}
	if _, err := m.CancelTask(context.Background(), parent, first.TaskID); err != nil {
		t.Fatal(err)
	}
	view = waitBackground(t, m, parent, first.TaskID, "cancelled")
	if !view.CancelRequested || calls.Load() != 1 {
		t.Fatalf("cancel replayed queued task: calls=%d %+v", calls.Load(), view)
	}
	if _, err := m.CancelTask(context.Background(), parent, first.TaskID); err != nil {
		t.Fatal("cancel not idempotent", err)
	}
}

func TestBackgroundFollowupRestoresHistoryAndSharesSessionLock(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := &transcriptModel{next: func(ctx context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		if calls.Add(1) == 1 {
			return llm.Turn{Content: "ORIGINAL_FINDINGS"}, nil
		}
		snapshot, err := conv.Snapshot()
		if err != nil {
			return llm.Turn{}, err
		}
		if !strings.Contains(string(snapshot.Messages), "ORIGINAL_FINDINGS") || !strings.Contains(string(snapshot.Messages), "extra detail") {
			return llm.Turn{}, errors.New("history missing")
		}
		close(entered)
		select {
		case <-release:
			return llm.Turn{Content: "continued"}, nil
		case <-ctx.Done():
			return llm.Turn{}, ctx.Err()
		}
	}}
	tasks := &memoryTaskStore{}
	m := backgroundManager(t, tasks, &continuationMemoryStore{})
	parent := managerParent(model)
	initial, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "history"})
	if err != nil {
		t.Fatal(err)
	}
	first := waitBackground(t, m, parent, initial.TaskID, "completed")
	deadline := time.Now().Add(time.Second)
	for !first.Result.CanFollowUp && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		first, err = m.GetTask(context.Background(), parent, initial.TaskID)
		if err != nil {
			t.Fatal(err)
		}
	}
	handle, err := m.SubmitFollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.SubagentID, Message: "extra detail", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := m.FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.SubagentID, Message: "racing"}); err == nil {
		t.Fatal("concurrent sync followup accepted")
	}
	items, err := m.ListSubagents(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TaskID != handle.TaskID || items[0].CanFollowUp {
		t.Fatalf("active task not discoverable: %+v", items)
	}
	close(release)
	view := waitBackground(t, m, parent, handle.TaskID, "completed")
	if view.SubagentID != first.SubagentID || view.PreviousRunID != first.RunID || view.RunID == first.RunID || view.Result.Summary != "continued" {
		t.Fatalf("identity/history lost: %+v", view)
	}
}

func TestBackgroundSubmissionFailureReleasesCapacityAndShutdownRejectsNewWork(t *testing.T) {
	tasks := &memoryTaskStore{createErr: errors.New("database unavailable")}
	m := backgroundManager(t, tasks, &continuationMemoryStore{})
	parent := managerParent(&transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "done"}, nil
	}})
	for i := 0; i < 4; i++ {
		if _, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "rejected"}); err == nil || errors.Is(err, ErrBackgroundFull) {
			t.Fatalf("reservation leaked: %v", err)
		}
	}
	tasks.mu.Lock()
	tasks.createErr = nil
	tasks.mu.Unlock()
	handle, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	waitBackground(t, m, parent, handle.TaskID, "completed")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.CloseBackground(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "late"}); !errors.Is(err, ErrBackgroundUnavailable) {
		t.Fatalf("accepted after shutdown: %v", err)
	}
}

func TestBackgroundPersistenceFailureIsVisibleWithoutFabricatingSuccess(t *testing.T) {
	tasks := &memoryTaskStore{finishErr: errors.New("disk unavailable")}
	m := backgroundManager(t, tasks, &continuationMemoryStore{})
	parent := managerParent(&transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "real result"}, nil
	}})
	handle, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	view := waitBackground(t, m, parent, handle.TaskID, "failed")
	if !strings.Contains(view.Error, "not persisted") {
		t.Fatalf("failure hidden: %+v", view)
	}
	tasks.mu.Lock()
	_, saved := tasks.runs[view.RunID]
	tasks.mu.Unlock()
	if saved {
		t.Fatal("reported persisted Run after transaction failure")
	}
}

func TestBackgroundRestartInterruptsAndDoesNotReplayUnfinishedFollowup(t *testing.T) {
	sessions := &continuationMemoryStore{}
	model := &transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "checkpoint"}, nil
	}}
	parent := managerParent(model)
	initial := testManager(t, Config{}).WithSubagentSessions(sessions)
	first, err := initial.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := strings.Repeat("a", 32)
	tasks := &memoryTaskStore{rows: map[string]store.SubagentTask{id: {ID: id, SubagentID: first.State.SubagentID, PreviousRunID: first.State.RunID, ConversationID: parent.ConversationID, Status: "running", StartedAt: &now}}}
	m := backgroundManager(t, tasks, sessions)
	view, err := m.GetTask(context.Background(), parent, id)
	if err != nil || view.Status != "interrupted" {
		t.Fatalf("bad restart state: %+v %v", view, err)
	}
	if _, err := m.FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"}); !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("stale checkpoint silently resumed: %v", err)
	}
}

func TestBackgroundParentCanAnswerAnotherQuestionAndRetrieveResultWithoutRebilling(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var parentCalls atomic.Int32
	var taskID string
	model := &transcriptModel{next: func(ctx context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		snapshot, err := conv.Snapshot()
		if err != nil {
			return llm.Turn{}, err
		}
		messages := string(snapshot.Messages)
		if strings.Contains(messages, "你负责完成明确委派的任务") {
			close(entered)
			select {
			case <-release:
				return llm.Turn{Content: "BACKGROUND_FINDINGS", Usage: llm.Usage{PromptTokens: 100}}, nil
			case <-ctx.Done():
				return llm.Turn{}, ctx.Err()
			}
		}
		if strings.Contains(messages, "ROOT_START") && parentCalls.Add(1) == 1 {
			return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "delegate", Name: runtime.DelegateTaskToolName, Arguments: `{"task":"independent research","background":true}`}}, Usage: llm.Usage{PromptTokens: 3}}, nil
		}
		if strings.Contains(messages, "ROOT_RESULT") && !strings.Contains(messages, "BACKGROUND_FINDINGS") {
			args, _ := json.Marshal(map[string]string{"task_id": taskID})
			return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "get_result", Name: runtime.GetSubagentTaskToolName, Arguments: string(args)}}, Usage: llm.Usage{PromptTokens: 5}}, nil
		}
		return llm.Turn{Content: "parent answer", Usage: llm.Usage{PromptTokens: 7}}, nil
	}}
	m := backgroundManager(t, &memoryTaskStore{}, &continuationMemoryStore{})
	parent := managerParent(model)
	first, err := runtime.Run(context.Background(), parent.Agent, runtime.Request{ConversationID: parent.ConversationID, Input: "ROOT_START", Delegator: m})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.State.Children) != 0 || len(first.State.AllCalls()) != 2 {
		t.Fatalf("background mutated parent accounting: %+v", first.State)
	}
	var handle runtime.BackgroundTask
	if err := json.Unmarshal([]byte(first.State.Steps[0].Result), &handle); err != nil || handle.TaskID == "" {
		t.Fatalf("missing receipt: %v", err)
	}
	taskID = handle.TaskID
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("child not started")
	}
	second, err := runtime.Run(context.Background(), parent.Agent, runtime.Request{ConversationID: parent.ConversationID, Input: "ROOT_OTHER", Delegator: m})
	if err != nil || second.Content != "parent answer" {
		t.Fatalf("main conversation blocked: %+v %v", second, err)
	}
	close(release)
	waitBackground(t, m, parent, taskID, "completed")
	third, err := runtime.Run(context.Background(), parent.Agent, runtime.Request{ConversationID: parent.ConversationID, Input: "ROOT_RESULT", Delegator: m})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.State.Children) != 0 || len(third.State.Calls) != 2 || len(third.State.AllCalls()) != 2 || !strings.Contains(third.State.Steps[0].Result, "BACKGROUND_FINDINGS") {
		t.Fatalf("result retrieval replayed/billed history: %+v", third.State)
	}
}

func TestBackgroundKeepsBranchReadScopeAndPanicDoesNotKillWorker(t *testing.T) {
	var calls atomic.Int32
	model := &transcriptModel{next: func(_ context.Context, _ *llm.Conversation, schemas []llm.ToolSchema) (llm.Turn, error) {
		for _, schema := range schemas {
			if schema.Name == "search_memories" || schema.Name == "get_sessions" {
				return llm.Turn{}, errors.New("global reader escaped branch scope")
			}
		}
		switch calls.Add(1) {
		case 1:
			return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "prefix", Name: "get_messages", Arguments: `{"session_id":10,"seq_start":1,"seq_end":3}`}, {ID: "other", Name: "get_messages", Arguments: `{"session_id":99,"seq_start":1,"seq_end":1}`}}}, nil
		case 2:
			return llm.Turn{Content: "branch findings"}, nil
		case 3:
			panic("a tool/model panic")
		default:
			return llm.Turn{Content: "worker survived"}, nil
		}
	}}
	tasks := &memoryTaskStore{}
	m := backgroundManager(t, tasks, &continuationMemoryStore{})
	parent := managerParent(model)
	parent.Agent.Abilities = []*agent.Ability{ability.NewBranchContextAbility([]store.ContextMessage{{SessionID: 10, Seq: 1, Role: "user", Content: "frozen prefix"}})}
	handle, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "branch research"})
	if err != nil {
		t.Fatal(err)
	}
	view := waitBackground(t, m, parent, handle.TaskID, "completed")
	tasks.mu.Lock()
	row := tasks.rows[view.TaskID]
	tasks.mu.Unlock()
	var result runtime.RunResult
	if err := json.Unmarshal(row.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.State.Steps) != 2 || !strings.Contains(result.State.Steps[0].Result, "frozen prefix") || strings.Contains(result.State.Steps[0].Result, `"seq":2`) || result.State.Steps[1].Error == "" {
		t.Fatalf("scope escaped: %+v", result.State.Steps)
	}
	panicked, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "panic"})
	if err != nil {
		t.Fatal(err)
	}
	waitBackground(t, m, parent, panicked.TaskID, "interrupted")
	survived, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if last := waitBackground(t, m, parent, survived.TaskID, "completed"); last.Result == nil || last.Result.Summary != "worker survived" {
		t.Fatalf("worker did not recover: %+v", last)
	}
}

func TestBackgroundShutdownFinishesRunningAndQueuedTasks(t *testing.T) {
	entered := make(chan struct{})
	model := &transcriptModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		close(entered)
		<-ctx.Done()
		return llm.Turn{}, ctx.Err()
	}}
	m := backgroundManager(t, &memoryTaskStore{}, &continuationMemoryStore{})
	parent := managerParent(model)
	first, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "running"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	second, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.CloseBackground(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.TaskID, second.TaskID} {
		view, err := m.GetTask(context.Background(), parent, id)
		if err != nil || view.Status != "interrupted" {
			t.Fatalf("unfinished shutdown record: %+v %v", view, err)
		}
	}
}

func TestCancellingQueuedFollowupReleasesSessionForExplicitRetry(t *testing.T) {
	entered := make(chan struct{})
	model := &transcriptModel{next: func(ctx context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		snapshot, err := conv.Snapshot()
		if err != nil {
			return llm.Turn{}, err
		}
		if strings.Contains(string(snapshot.Messages), "OCCUPY_WORKER") {
			close(entered)
			<-ctx.Done()
			return llm.Turn{}, ctx.Err()
		}
		return llm.Turn{Content: "checkpoint"}, nil
	}}
	m := backgroundManager(t, &memoryTaskStore{}, &continuationMemoryStore{})
	parent := managerParent(model)
	initial, err := m.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "history"})
	if err != nil {
		t.Fatal(err)
	}
	occupier, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "OCCUPY_WORKER"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	pending, err := m.SubmitFollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: initial.State.SubagentID, Message: "queued request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CancelTask(context.Background(), parent, pending.TaskID); err != nil {
		t.Fatal(err)
	}
	continued, err := m.FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: initial.State.SubagentID, Message: "explicit retry"})
	if err != nil || continued.State.PreviousRunID != initial.State.RunID {
		t.Fatalf("cancelled queue held the session: %+v %v", continued, err)
	}
	if _, err := m.CancelTask(context.Background(), parent, occupier.TaskID); err != nil {
		t.Fatal(err)
	}
}
