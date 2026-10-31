package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

type continuationMemoryStore struct {
	mu              sync.Mutex
	rows            map[string]store.SubagentSession
	updateError     error
	checkpointError error
}

func cloneSession(row store.SubagentSession) store.SubagentSession {
	row.Snapshot = append(json.RawMessage(nil), row.Snapshot...)
	row.PresetAgent = append(json.RawMessage(nil), row.PresetAgent...)
	row.Sources = append(json.RawMessage(nil), row.Sources...)
	return row
}

func (s *continuationMemoryStore) Create(_ context.Context, row *store.SubagentSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = make(map[string]store.SubagentSession)
	}
	if _, ok := s.rows[row.ID]; ok {
		return errors.New("duplicate session")
	}
	row.Revision = 1
	s.rows[row.ID] = cloneSession(*row)
	return nil
}
func (s *continuationMemoryStore) Load(_ context.Context, id string, owner uint) (store.SubagentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.ConversationID != owner {
		return store.SubagentSession{}, errors.New("not found")
	}
	return cloneSession(row), nil
}
func (s *continuationMemoryStore) Update(_ context.Context, row *store.SubagentSession, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateError != nil {
		return s.updateError
	}
	old, ok := s.rows[row.ID]
	if !ok || old.ConversationID != row.ConversationID || old.Revision != revision {
		return ErrSessionConflict
	}
	row.Revision = revision + 1
	s.rows[row.ID] = cloneSession(*row)
	return nil
}
func (s *continuationMemoryStore) List(_ context.Context, owner uint, _ int) ([]store.SubagentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []store.SubagentSession
	for _, row := range s.rows {
		if row.ConversationID == owner {
			rows = append(rows, cloneSession(row))
		}
	}
	return rows, nil
}
func (s *continuationMemoryStore) CheckCheckpoint(context.Context, store.SubagentSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointError
}

type transcriptModel struct {
	next func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error)
}

func (m *transcriptModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected one-turn model")
}
func (m *transcriptModel) Next(ctx context.Context, conv *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
	turn, err := m.next(ctx, conv, tools)
	if err == nil {
		conv.AddAssistantTurn(turn)
	}
	return turn, err
}

func TestContinuationRestoresRealHistoryAcrossManagersWithoutRepeatingToolsOrUsage(t *testing.T) {
	repository := &continuationMemoryStore{}
	modelCalls, reads := 0, 0
	model := &transcriptModel{next: func(_ context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		modelCalls++
		if modelCalls == 1 {
			return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "original_call", Name: "get_messages", Arguments: `{"session_id":8,"seq_start":3,"seq_end":3}`}}, Usage: llm.Usage{PromptTokens: 10}}, nil
		}
		if modelCalls == 2 {
			return llm.Turn{Content: "original findings", Usage: llm.Usage{PromptTokens: 20}}, nil
		}
		snapshot, err := conv.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range []string{"original_call", "ORIGINAL_TOOL_BODY", "original findings", "补查最新讨论"} {
			if !strings.Contains(string(snapshot.Messages), fragment) {
				t.Fatalf("missing prior history %q", fragment)
			}
		}
		return llm.Turn{Content: "continued findings", Usage: llm.Usage{PromptTokens: 30}}, nil
	}}
	parent := managerParent(model, managerReadTool("get_messages", func(context.Context, string) (string, error) {
		reads++
		return `{"session_id":8,"messages":[{"seq":3,"content":"ORIGINAL_TOOL_BODY"}]}`, nil
	}))
	first, err := testManager(t, Config{}).WithSubagentSessions(repository).Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "核查历史"})
	if err != nil {
		t.Fatal(err)
	}
	if first.State.SubagentID == "" || first.State.ContextSnapshot == nil {
		t.Fatal("initial child is not resumable")
	}
	parent.RunID = "parent-two"
	manager := testManager(t, Config{}).WithSubagentSessions(repository)
	second, err := manager.FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "补查最新讨论"})
	if err != nil {
		t.Fatal(err)
	}
	if second.State.SubagentID != first.State.SubagentID || second.State.RunID == first.State.RunID || second.State.PreviousRunID != first.State.RunID || second.State.ParentRunID != "parent-two" {
		t.Fatalf("identity chain lost: %+v", second.State)
	}
	if reads != 1 || len(second.State.Steps) != 0 || len(second.State.Calls) != 1 || len(second.State.AllCalls()) != 1 || second.State.Calls[0].Usage.PromptTokens != 30 {
		t.Fatalf("old tools or usage replayed: reads=%d state=%+v", reads, second.State)
	}
	sources := runtime.SourcesForRun(second.State)
	if len(sources) != 1 || sources[0].SourceRunID != first.State.RunID || sources[0].SeqStart != 3 {
		t.Fatalf("historical source lost: %+v", sources)
	}
	items, err := manager.ListSubagents(context.Background(), parent)
	if err != nil || len(items) != 1 || items[0].LastRunID != second.State.RunID || !items[0].CanFollowUp {
		t.Fatalf("latest child not discoverable: %+v %v", items, err)
	}
	row, _ := repository.Load(context.Background(), first.State.SubagentID, parent.ConversationID)
	if row.Revision != 2 {
		t.Fatalf("checkpoint not advanced: %+v", row)
	}
}

func TestContinuationChecksOwnerAndRemovedPresetAgentBeforeCallingModel(t *testing.T) {
	repository := &continuationMemoryStore{}
	calls := 0
	model := &transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls++
		return llm.Turn{Content: "done"}, nil
	}}
	parent := managerParent(model)
	manager := testManager(t, Config{}).WithSubagentSessions(repository)
	first, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := parent
	foreign.ConversationID++
	if result, err := manager.FollowUp(context.Background(), foreign, runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"}); err == nil || result.State.RunID != "" {
		t.Fatal("foreign conversation resumed child")
	}
	otherPresetAgent := PresetAgent{Name: "other", Description: "other role", Instructions: "other instructions", AllowedTools: []string{"get_messages"}}
	changed := testManager(t, Config{PresetAgents: []PresetAgent{otherPresetAgent}, DefaultAgent: "other"}).WithSubagentSessions(repository)
	if result, err := changed.FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"}); err == nil || result.State.RunID != "" {
		t.Fatal("removed role resumed")
	}
	if calls != 1 {
		t.Fatalf("unauthorized continuation called model %d times", calls)
	}
}

func TestContinuationCannotExpandOriginalToolAndBudgetGrant(t *testing.T) {
	repository := &continuationMemoryStore{}
	call := 0
	model := &transcriptModel{next: func(ctx context.Context, _ *llm.Conversation, tools []llm.ToolSchema) (llm.Turn, error) {
		call++
		if call == 1 {
			return llm.Turn{Content: "initial"}, nil
		}
		for _, tool := range tools {
			if tool.Name != "get_messages" && tool.Name != "update_plan" {
				t.Fatalf("expanded tool grant: %s", tool.Name)
			}
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 1500*time.Millisecond {
			t.Fatal("expanded timeout")
		}
		return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "read", Name: "get_messages", Arguments: `{}`}}}, nil
	}}
	parent := managerParent(model, managerReadTool("get_messages", func(context.Context, string) (string, error) { return `{"session_id":8,"messages":[]}`, nil }), managerReadTool("search_memories", nil))
	PresetAgent := PresetAgent{Name: "auditor", Description: "audit", Instructions: "old role", AllowedTools: []string{"get_messages"}, MaxRounds: 2, Timeout: time.Second}
	first, err := testManager(t, Config{PresetAgents: []PresetAgent{PresetAgent}, DefaultAgent: "auditor"}).WithSubagentSessions(repository).Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "audit"})
	if err != nil {
		t.Fatal(err)
	}
	PresetAgent.MaxRounds = 8
	PresetAgent.Timeout = 2 * time.Second
	PresetAgent.AllowedTools = append(PresetAgent.AllowedTools, "search_memories")
	continued, err := testManager(t, Config{PresetAgents: []PresetAgent{PresetAgent}, DefaultAgent: "auditor"}).WithSubagentSessions(repository).FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"})
	if err == nil || continued.State.CurrentRound != 2 || call != 3 {
		t.Fatalf("old round budget was expanded: calls=%d run=%+v err=%v", call, continued.State, err)
	}
}

func TestContinuationRejectsConcurrentExecutionOfSameChild(t *testing.T) {
	repository := &continuationMemoryStore{}
	var calls atomic.Int32
	ready, release := make(chan struct{}), make(chan struct{})
	model := &transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		if calls.Add(1) == 2 {
			close(ready)
			<-release
		}
		return llm.Turn{Content: "done"}, nil
	}}
	parent := managerParent(model)
	manager := testManager(t, Config{}).WithSubagentSessions(repository)
	first, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	request := runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"}
	done := make(chan error, 1)
	go func() { _, err := manager.FollowUp(context.Background(), parent, request); done <- err }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("continuation did not start")
	}
	result, secondErr := manager.FollowUp(context.Background(), parent, request)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if secondErr == nil || result.State.RunID != "" || calls.Load() != 2 {
		t.Fatal("concurrent continuation started a second model call")
	}
}

func TestContinuationWriteFailureCannotSilentlyReuseOldCheckpoint(t *testing.T) {
	repository := &continuationMemoryStore{}
	calls := 0
	model := &transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls++
		return llm.Turn{Content: "done"}, nil
	}}
	parent := managerParent(model)
	manager := testManager(t, Config{}).WithSubagentSessions(repository)
	first, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	repository.updateError = errors.New("database unavailable")
	request := runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"}
	failed, err := manager.FollowUp(context.Background(), parent, request)
	if err == nil || failed.State.ContextSnapshot != nil || !failed.State.ContextPersistFailed || len(failed.State.Calls) != 1 {
		t.Fatal("failed save hid actual execution or exposed stale continuation")
	}
	repository.updateError = nil
	if _, err := manager.FollowUp(context.Background(), parent, request); !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("old checkpoint was reused: %v", err)
	}
	items, err := manager.ListSubagents(context.Background(), parent)
	if err != nil || len(items) != 1 || items[0].CanFollowUp || calls != 2 {
		t.Fatalf("failed child still advertised as resumable: %+v calls=%d err=%v", items, calls, err)
	}
	// After restart, persisted Run metadata is the guard supplied by the DB
	// adapter; this fake simulates the same CheckpointVerifier result.
	repository.checkpointError = ErrCheckpointUnavailable
	restarted := testManager(t, Config{}).WithSubagentSessions(repository)
	if _, err := restarted.FollowUp(context.Background(), parent, request); !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("restart resumed uncommitted work: %v", err)
	}
}

func TestContinuationCASConflictDoesNotPoisonNewerCheckpoint(t *testing.T) {
	repository := &continuationMemoryStore{}
	model := &transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "done"}, nil
	}}
	parent := managerParent(model)
	manager := testManager(t, Config{}).WithSubagentSessions(repository)
	first, err := manager.Delegate(context.Background(), parent, runtime.DelegationRequest{Task: "task"})
	if err != nil {
		t.Fatal(err)
	}
	repository.updateError = ErrSessionConflict
	failed, err := manager.FollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.State.SubagentID, Message: "continue"})
	if !errors.Is(err, ErrSessionConflict) || failed.State.ContextPersistFailed {
		t.Fatal("CAS conflict marked checkpoint as lost")
	}
	repository.updateError = nil
	items, err := manager.ListSubagents(context.Background(), parent)
	if err != nil || len(items) != 1 || !items[0].CanFollowUp {
		t.Fatal("a competing authoritative checkpoint was disabled")
	}
}
