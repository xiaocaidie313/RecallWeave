package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/delivery"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

type memoryGroupStore struct {
	memoryTaskStore
	groups           map[string]store.TaskGroup
	announced        map[string]int
	groupCreateError error
}

func (s *memoryGroupStore) GroupsEnabled() bool { return true }
func (s *memoryGroupStore) CreateGroup(ctx context.Context, group *store.TaskGroup, rows []store.SubagentTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.groupCreateError != nil {
		return s.groupCreateError
	}
	if len(rows) != 2 {
		return errors.New("two members required")
	}
	if s.groups == nil {
		s.groups = make(map[string]store.TaskGroup)
	}
	if s.rows == nil {
		s.rows = make(map[string]store.SubagentTask)
	}
	if _, exists := s.groups[group.ID]; exists {
		return errors.New("duplicate")
	}
	if group.DeliveryMode == delivery.Direct {
		group.ReportStatus = "not_required"
	}
	s.groups[group.ID] = *group
	for _, row := range rows {
		s.rows[row.ID] = cloneTask(row)
	}
	return nil
}
func (s *memoryGroupStore) groupLocked(id string, owner uint) (store.TaskGroup, []store.SubagentTask, error) {
	group, ok := s.groups[id]
	if !ok || group.ConversationID != owner {
		return store.TaskGroup{}, nil, ErrTaskNotFound
	}
	var ids []string
	if json.Unmarshal(group.TaskIDs, &ids) != nil {
		return group, nil, errors.New("bad membership")
	}
	rows := make([]store.SubagentTask, 0, len(ids))
	for _, taskID := range ids {
		row, ok := s.rows[taskID]
		if !ok || row.GroupID != id || row.ConversationID != owner {
			return group, nil, ErrTaskNotFound
		}
		rows = append(rows, cloneTask(row))
	}
	return group, rows, nil
}
func (s *memoryGroupStore) LoadGroup(ctx context.Context, id string, owner uint) (store.TaskGroup, []store.SubagentTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.TaskGroup{}, nil, err
	}
	return s.groupLocked(id, owner)
}
func (s *memoryGroupStore) ListGroups(ctx context.Context, owner uint, limit int) ([]store.TaskGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []store.TaskGroup
	for _, group := range s.groups {
		if group.ConversationID == owner {
			rows = append(rows, group)
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}
func (s *memoryGroupStore) refreshLocked(id string) {
	if id == "" {
		return
	}
	group, rows, err := s.groupLocked(id, s.groups[id].ConversationID)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row.Status == "queued" || row.Status == "running" {
			return
		}
	}
	if group.Status != "running" {
		return
	}
	successes, cancelled, interrupted := 0, 0, 0
	requested := group.CancelRequested
	for _, row := range rows {
		if row.Status == "completed" || row.Status == "partial" {
			successes++
		}
		if row.Status == "cancelled" {
			cancelled++
		}
		if row.Status == "interrupted" {
			interrupted++
		}
		requested = requested || row.CancelRequested
	}
	group.Status = "failed"
	if rows[0].Status == "completed" && rows[1].Status == "completed" {
		group.Status = "completed"
	} else if successes > 0 {
		group.Status = "partial"
	} else if cancelled == 2 && requested {
		group.Status = "cancelled"
	} else if interrupted == 2 {
		group.Status = "interrupted"
	}
	now := time.Now()
	group.FinishedAt = &now
	group.UpdatedAt = now
	if group.DeliveryMode == delivery.Direct {
		group.ReportStatus = "not_required"
	} else {
		group.ReportStatus = "pending"
	}
	s.groups[id] = group
	if s.announced == nil {
		s.announced = make(map[string]int)
	}
	s.announced[id]++
}
func (s *memoryGroupStore) Finish(ctx context.Context, row *store.SubagentTask, run *store.AgentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.finishErr != nil {
		return s.finishErr
	}
	old, ok := s.rows[row.ID]
	if !ok || old.ConversationID != row.ConversationID {
		return ErrTaskNotFound
	}
	if old.Status != "queued" && old.Status != "running" {
		return ErrTaskConflict
	}
	row.CancelRequested = old.CancelRequested
	if row.StartedAt == nil {
		row.StartedAt = old.StartedAt
	}
	if row.GroupID != "" {
		if s.groups[row.GroupID].DeliveryMode == delivery.Direct {
			row.ReportStatus = "pending"
		} else {
			row.ReportStatus = "grouped"
		}
	}
	s.rows[row.ID] = cloneTask(*row)
	if run != nil {
		if s.runs == nil {
			s.runs = make(map[string]store.AgentRun)
		}
		s.runs[run.RunID] = *run
	}
	s.refreshLocked(row.GroupID)
	return nil
}
func (s *memoryGroupStore) RequestCancelGroup(ctx context.Context, id string, owner uint) (store.TaskGroup, []store.SubagentTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.TaskGroup{}, nil, err
	}
	group, rows, err := s.groupLocked(id, owner)
	if err != nil {
		return group, rows, err
	}
	if group.Status == "running" {
		group.CancelRequested = true
		s.groups[id] = group
		for _, row := range rows {
			if row.Status == "queued" || row.Status == "running" {
				row.CancelRequested = true
				if row.Status == "queued" {
					row.Status = "cancelled"
					if group.DeliveryMode == delivery.Direct {
						row.ReportStatus = "pending"
					}
					now := time.Now()
					row.FinishedAt = &now
				}
				s.rows[row.ID] = row
			}
		}
		s.refreshLocked(id)
	}
	return s.groupLocked(id, owner)
}
func (s *memoryGroupStore) InterruptUnfinished(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, row := range s.rows {
		if row.Status == "queued" || row.Status == "running" {
			now := time.Now()
			row.Status = "interrupted"
			if row.GroupID != "" && s.groups[row.GroupID].DeliveryMode == delivery.Direct {
				row.ReportStatus = "pending"
			}
			row.FinishedAt = &now
			s.rows[id] = row
		}
	}
	for id := range s.groups {
		s.refreshLocked(id)
	}
	return nil
}

func groupManager(t *testing.T, repository *memoryGroupStore, workers, queue int) *Manager {
	t.Helper()
	m := testManager(t, Config{}).WithSubagentSessions(&continuationMemoryStore{})
	if err := m.StartBackground(context.Background(), repository, BackgroundConfig{Workers: workers, QueueCapacity: queue}); err != nil {
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
func groupRequest() runtime.GroupDelegationRequest {
	return runtime.GroupDelegationRequest{Goal: "compare independent evidence", Context: "Do not access unrelated conversations", ExpectedOutput: "One comparison with sources and gaps", DeliveryMode: delivery.Synthesize, Tasks: []runtime.GroupTask{{Task: "MEMBER_A"}, {Task: "MEMBER_B"}}}
}
func waitGroup(t *testing.T, m *Manager, parent runtime.DelegationParent, id, status string) runtime.TaskGroup {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, err := m.GetGroup(context.Background(), parent, id)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status == status {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("group %s did not become %s", id, status)
	return runtime.TaskGroup{}
}

func TestGroupExecutesTwoIsolatedChildrenAndWaitsForBoth(t *testing.T) {
	entered := make(chan string, 2)
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	model := &transcriptModel{next: func(ctx context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		snapshot, err := conv.Snapshot()
		if err != nil {
			return llm.Turn{}, err
		}
		body := string(snapshot.Messages)
		if !strings.Contains(body, "compare independent evidence") || !strings.Contains(body, "Do not access unrelated conversations") {
			return llm.Turn{}, errors.New("common goal or constraints lost")
		}
		wait := releaseA
		name := "A"
		if strings.Contains(body, "MEMBER_B") {
			wait = releaseB
			name = "B"
		}
		if strings.Contains(body, "MEMBER_A") && strings.Contains(body, "MEMBER_B") {
			return llm.Turn{}, errors.New("sibling context leaked")
		}
		entered <- name
		select {
		case <-wait:
			return llm.Turn{Content: name + " findings", Usage: llm.Usage{PromptTokens: 11}}, nil
		case <-ctx.Done():
			return llm.Turn{}, ctx.Err()
		}
	}}
	repository := &memoryGroupStore{}
	m := groupManager(t, repository, 2, 1)
	parent := managerParent(model)
	handle, err := m.SubmitGroup(context.Background(), parent, groupRequest())
	if err != nil {
		t.Fatal(err)
	}
	if handle.Status != "running" || len(handle.Tasks) != 2 || handle.Tasks[0].SubagentID == handle.Tasks[1].SubagentID {
		t.Fatalf("invalid group receipt: %+v", handle)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("children did not overlap")
		}
	}
	close(releaseA)
	waitBackground(t, m, parent, handle.TaskIDs[0], "completed")
	view, err := m.GetGroup(context.Background(), parent, handle.GroupID)
	if err != nil || view.Status != "running" || view.ReportStatus != "" {
		t.Fatalf("group reported first child early: %+v %v", view, err)
	}
	close(releaseB)
	view = waitGroup(t, m, parent, handle.GroupID, "completed")
	if view.ReportStatus != "pending" || view.Tasks[0].ReportStatus != "grouped" || view.Tasks[1].ReportStatus != "grouped" {
		t.Fatalf("individual reports escaped: %+v", view)
	}
	repository.mu.Lock()
	announced := repository.announced[handle.GroupID]
	runs := len(repository.runs)
	repository.mu.Unlock()
	if announced != 1 || runs != 2 {
		t.Fatalf("duplicate/missing completion: announcements=%d runs=%d", announced, runs)
	}
	if _, err := m.GetGroup(context.Background(), runtime.DelegationParent{ConversationID: parent.ConversationID + 1}, handle.GroupID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("foreign group readable: %v", err)
	}
}

func TestGroupRetainsSuccessfulResultWhenSiblingFails(t *testing.T) {
	model := &transcriptModel{next: func(_ context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		snapshot, _ := conv.Snapshot()
		if strings.Contains(string(snapshot.Messages), "MEMBER_B") {
			return llm.Turn{}, errors.New("external lookup failed")
		}
		return llm.Turn{Content: "ACTUAL_A_EVIDENCE"}, nil
	}}
	m := groupManager(t, &memoryGroupStore{}, 2, 1)
	parent := managerParent(model)
	handle, err := m.SubmitGroup(context.Background(), parent, groupRequest())
	if err != nil {
		t.Fatal(err)
	}
	view := waitGroup(t, m, parent, handle.GroupID, "partial")
	if view.Tasks[0].Result == nil || view.Tasks[0].Result.Summary != "ACTUAL_A_EVIDENCE" || view.Tasks[1].Status != "failed" || !strings.Contains(view.Tasks[1].Error, "external lookup failed") {
		t.Fatalf("partial evidence lost: %+v", view)
	}
}

func TestGroupCancellationWaitsForRunningChildAndSkipsQueuedChild(t *testing.T) {
	entered, cancelSeen, allowExit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := &transcriptModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancelSeen)
		<-allowExit
		return llm.Turn{}, ctx.Err()
	}}
	m := groupManager(t, &memoryGroupStore{}, 1, 1)
	parent := managerParent(model)
	handle, err := m.SubmitGroup(context.Background(), parent, groupRequest())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	view, err := m.CancelGroup(context.Background(), parent, handle.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.CancelRequested || view.Status != "running" {
		t.Fatalf("cancellation falsely finished running work: %+v", view)
	}
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach running child")
	}
	close(allowExit)
	waitGroup(t, m, parent, handle.GroupID, "cancelled")
	if calls.Load() != 1 {
		t.Fatalf("queued child was executed: %d", calls.Load())
	}
}

func TestGroupValidatesAllRolesAndReservesAllCapacityBeforeCreating(t *testing.T) {
	model := &transcriptModel{next: func(ctx context.Context, _ *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		<-ctx.Done()
		return llm.Turn{}, ctx.Err()
	}}
	repository := &memoryGroupStore{}
	m := groupManager(t, repository, 1, 1)
	parent := managerParent(model)
	bad := groupRequest()
	bad.Tasks[1].Agent = "missing"
	if _, err := m.SubmitGroup(context.Background(), parent, bad); err == nil {
		t.Fatal("unregistered role accepted")
	}
	occupier, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "single"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := m.SubmitGroup(context.Background(), parent, groupRequest()); !errors.Is(err, ErrBackgroundFull) || receipt.GroupID != "" {
		t.Fatalf("partially accepted full group: %+v %v", receipt, err)
	}
	repository.mu.Lock()
	count := len(repository.groups)
	tasks := len(repository.rows)
	repository.mu.Unlock()
	if count != 0 || tasks != 1 {
		t.Fatalf("partially persisted group: groups=%d tasks=%d", count, tasks)
	}
	if _, err := m.CancelTask(context.Background(), parent, occupier.TaskID); err != nil {
		t.Fatal(err)
	}
}

func TestGroupCreationFailureReleasesSlotsAndExecutionLocks(t *testing.T) {
	repository := &memoryGroupStore{groupCreateError: errors.New("database unavailable")}
	m := groupManager(t, repository, 1, 1)
	parent := managerParent(&transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "done"}, nil
	}})
	for i := 0; i < 3; i++ {
		if _, err := m.SubmitGroup(context.Background(), parent, groupRequest()); err == nil || errors.Is(err, ErrBackgroundFull) {
			t.Fatalf("group leaked reservation: %v", err)
		}
	}
	m.activeMu.Lock()
	locks := len(m.active)
	m.activeMu.Unlock()
	if locks != 0 {
		t.Fatalf("execution locks leaked: %d", locks)
	}
	repository.mu.Lock()
	repository.groupCreateError = nil
	repository.mu.Unlock()
	handle, err := m.SubmitGroup(context.Background(), parent, groupRequest())
	if err != nil {
		t.Fatal(err)
	}
	waitGroup(t, m, parent, handle.GroupID, "completed")
}
