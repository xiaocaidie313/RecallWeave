package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

const sessionPolicyVersion = 1

type sessionPolicy struct {
	Version     int         `json:"version"`
	PresetAgent PresetAgent `json:"PresetAgent"`
	Tools       []string    `json:"tools"`
}

type unsavedCheckpoint struct {
	Revision uint64
	RunID    string
}

var subagentIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

var _ runtime.ContinuationDelegator = (*Manager)(nil)

// WithSessions is called once during assembly, before serving requests. The
// repository persists data; live execution locks remain local to this Manager.
func (m *Manager) WithSubagentSessions(subagentSessionsStore SubagentSessionStore) *Manager {
	m.subagentSessionsStore = subagentSessionsStore
	return m
}

// 有无subagentSessions 来决定是否开启续期
func (m *Manager) ContinuationsEnabled() bool { return m != nil && m.subagentSessionsStore != nil }

func newSubagentID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("subagent: create identity: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// lockSession prevents concurrent continuations in this server process. The
// repository's revision check also prevents stale writes across Managers.
func (m *Manager) lockSubagentSession(conversationID uint, id string) (func(), error) {
	key := fmt.Sprintf("%d/%s", conversationID, id)
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if m.active[key] {
		return nil, fmt.Errorf("subagent: this child is already running")
	}
	m.active[key] = true
	return func() { m.activeMu.Lock(); delete(m.active, key); m.activeMu.Unlock() }, nil
}

func (m *Manager) FollowUp(ctx context.Context, parent runtime.DelegationParent, request runtime.FollowupRequest) (runtime.RunResult, error) {
	previous, current, release, err := m.prepareFollowUp(ctx, parent, request)
	if err != nil {
		return runtime.RunResult{}, err
	}
	defer release()
	return m.executeSession(ctx, parent, "继续此子任务：\n"+request.Message, previous.Task, current, previous.ID, &previous)
}

func (m *Manager) prepareFollowUp(ctx context.Context, parent runtime.DelegationParent, request runtime.FollowupRequest) (store.SubagentSession, PresetAgent, func(), error) {
	var empty store.SubagentSession
	fail := func(err error) (store.SubagentSession, PresetAgent, func(), error) {
		return empty, PresetAgent{}, nil, err
	}
	if !m.ContinuationsEnabled() || parent.Agent == nil || parent.Agent.Model == nil || parent.RunID == "" || parent.ConversationID == 0 {
		return fail(fmt.Errorf("subagent: continuation requires a live parent and conversation"))
	}
	if !subagentIDPattern.MatchString(request.SubagentID) || !utf8.ValidString(request.Message) || strings.TrimSpace(request.Message) == "" || len(request.Message) > 8<<10 {
		return fail(fmt.Errorf("subagent: invalid child identity or follow-up message"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	release, err := m.lockSubagentSession(parent.ConversationID, request.SubagentID)
	if err != nil {
		return fail(err)
	}
	prepared := false
	defer func() {
		if !prepared {
			release()
		}
	}()
	previous, err := m.subagentSessionsStore.Load(ctx, request.SubagentID, parent.ConversationID)
	if err != nil {
		return fail(fmt.Errorf("subagent: child unavailable in this conversation: %w", err))
	}
	// 如果不是同一任务/ 执行agent  或者不是同一对话
	if previous.ID != request.SubagentID || previous.ConversationID != parent.ConversationID {
		return fail(fmt.Errorf("subagent: child ownership mismatch"))
	}
	// 检查是否可以继续
	if err := m.checkCheckpoint(ctx, previous); err != nil {
		return fail(err)
	}
	current, ok := m.PresetAgents[previous.AgentName]
	if !ok {
		return fail(fmt.Errorf("subagent: the child's PresetAgent is no longer registered"))
	}
	// policy 限制哪些能行， 能用
	var policy sessionPolicy
	if json.Unmarshal(previous.PresetAgent, &policy) != nil || policy.Version != sessionPolicyVersion || policy.PresetAgent.Name != previous.AgentName || validateBudget(policy.PresetAgent.MaxRounds, policy.PresetAgent.Timeout) != nil {
		return fail(fmt.Errorf("subagent: saved child policy is invalid"))
	}
	// Current policy can narrow the old grant, never enlarge it. Recreated
	// tools come from this request's scoped parent, preserving branch readers.
	priorTools := make(map[string]bool, len(policy.Tools))
	for _, name := range policy.Tools {
		priorTools[name] = true
	}
	var tools []string
	for _, name := range current.AllowedTools {
		if priorTools[name] {
			tools = append(tools, name)
		}
	}
	// 跟新能用的tools
	current.AllowedTools = tools
	if policy.PresetAgent.MaxRounds < current.MaxRounds {
		current.MaxRounds = policy.PresetAgent.MaxRounds
	}
	if policy.PresetAgent.Timeout < current.Timeout {
		current.Timeout = policy.PresetAgent.Timeout
	}
	prepared = true
	return previous, current, release, nil
}

func (m *Manager) executeSession(ctx context.Context, parent runtime.DelegationParent, input, task string, PresetAgent PresetAgent, id string, previous *store.SubagentSession) (runtime.RunResult, error) {
	// 创建一个新的子agent
	child := m.newChild(parent.Agent, PresetAgent)
	return m.executeChildSession(ctx, child, parent, input, task, PresetAgent, id, previous)
}

func (m *Manager) executeChildSession(ctx context.Context, child *agent.Agent, parent runtime.DelegationParent, input, task string, PresetAgent PresetAgent, id string, previous *store.SubagentSession) (runtime.RunResult, error) {
	request := runtime.Request{ParentRunID: parent.RunID, ConversationID: parent.ConversationID, SubagentID: id, Input: input, CaptureContext: m.ContinuationsEnabled()}
	if previous != nil {
		if len(previous.Snapshot) == 0 || string(previous.Snapshot) == "null" {
			return runtime.RunResult{}, fmt.Errorf("subagent: this child has no resumable context")
		}
		var snapshot llm.ConversationSnapshot
		if err := json.Unmarshal(previous.Snapshot, &snapshot); err != nil {
			return runtime.RunResult{}, fmt.Errorf("subagent: invalid saved context")
		}
		request.ContextSnapshot = &snapshot
		request.PreviousRunID = previous.LatestRunID
		if err := json.Unmarshal(previous.Sources, &request.InheritedSources); err != nil {
			return runtime.RunResult{}, fmt.Errorf("subagent: invalid saved source records")
		}
	}
	childCtx, cancel := context.WithTimeout(ctx, PresetAgent.Timeout)
	defer cancel()
	result, runErr := runtime.Run(childCtx, child, request)
	if !m.ContinuationsEnabled() || result.State.RunID == "" {
		return result, runErr
	}
	policy := sessionPolicy{Version: sessionPolicyVersion, PresetAgent: PresetAgent}
	for _, schema := range child.Tools().Schemas() {
		policy.Tools = append(policy.Tools, schema.Name)
	}
	encodedPolicy, err := json.Marshal(policy)
	if err != nil {
		return result, err
	}
	encodedSnapshot, err := json.Marshal(result.State.ContextSnapshot)
	if err != nil {
		return result, err
	}
	encodedSources, err := json.Marshal(runtime.SourcesForRun(result.State))
	if err != nil {
		return result, err
	}
	// 新建 子agent session 记录
	record := store.SubagentSession{ID: id, ConversationID: parent.ConversationID, AgentName: PresetAgent.Name, Task: task, LatestRunID: result.State.RunID, Status: string(result.State.Status), Snapshot: encodedSnapshot, PresetAgent: encodedPolicy, Sources: encodedSources}
	// Save completed/failed work even if the HTTP request has been cancelled.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer saveCancel()
	if previous == nil {
		err = m.subagentSessionsStore.Create(saveCtx, &record)
	} else {
		err = m.subagentSessionsStore.Update(saveCtx, &record, previous.Revision)
	}
	// 写库有问题
	if err != nil {
		result.State.ContextSnapshot = nil
		result.State.ContextError = "subagent context was not saved: " + err.Error()
		// CAS means another version may already be authoritative. Ordinary
		// write failures instead mark this exact old checkpoint as unsafe.
		// 不是因为冲突 而是因为其他问题   比如超时或者 连不上
		if !errors.Is(err, ErrSessionConflict) {
			result.State.ContextPersistFailed = true
			if previous != nil {
				key := fmt.Sprintf("%d/%s", parent.ConversationID, id)
				m.activeMu.Lock()
				m.unsaved[key] = unsavedCheckpoint{Revision: previous.Revision, RunID: previous.LatestRunID}
				m.activeMu.Unlock()
			}
		}
		return result, errors.Join(runErr, fmt.Errorf("subagent: save continuation context: %w", err))
	}
	if result.State.ContextError != "" {
		return result, errors.Join(runErr, fmt.Errorf("subagent: context cannot be continued: %s", result.State.ContextError))
	}
	return result, runErr
}

func (m *Manager) ListSubagents(ctx context.Context, parent runtime.DelegationParent) ([]runtime.SubagentInfo, error) {
	if !m.ContinuationsEnabled() || parent.Agent == nil || parent.RunID == "" || parent.ConversationID == 0 {
		return nil, fmt.Errorf("subagent: listing requires a live parent conversation")
	}
	rows, err := m.subagentSessionsStore.List(ctx, parent.ConversationID, 5)
	if err != nil {
		return nil, err
	}
	items := make([]runtime.SubagentInfo, 0, len(rows))
	updated := make(map[string]time.Time)
	for _, row := range rows {
		if row.ConversationID != parent.ConversationID {
			return nil, fmt.Errorf("subagent: stored child ownership mismatch")
		}
		canContinue := len(row.Snapshot) > 0 && string(row.Snapshot) != "null"
		if err := m.checkCheckpoint(ctx, row); err != nil {
			if !errors.Is(err, ErrCheckpointUnavailable) {
				return nil, err
			}
			canContinue = false
		}
		_, registered := m.PresetAgents[row.AgentName]
		status := runtime.RunStatus(row.Status)
		m.activeMu.Lock()
		busy := m.active[fmt.Sprintf("%d/%s", parent.ConversationID, row.ID)]
		m.activeMu.Unlock()
		if busy {
			status = runtime.RunRunning
		}
		items = append(items, runtime.SubagentInfo{SubagentID: row.ID, Agent: row.AgentName, Task: shortTask(row.Task), LastRunID: row.LatestRunID, Status: status, CanFollowUp: canContinue && registered && !busy})
		updated[row.ID] = row.UpdatedAt
	}
	if m.BackgroundEnabled() {
		tasks, err := m.ListTasks(ctx, parent.ConversationID)
		if err != nil {
			return nil, err
		}
		positions := make(map[string]int, len(items))
		for i, item := range items {
			positions[item.SubagentID] = i
		}
		for _, task := range tasks {
			index, exists := positions[task.SubagentID]
			if exists && !task.UpdatedAt.After(updated[task.SubagentID]) {
				if items[index].TaskID == "" && task.RunID == items[index].LastRunID {
					items[index].TaskID = task.TaskID
				}
				continue
			}
			lastRunID := task.RunID
			if lastRunID == "" {
				lastRunID = task.PreviousRunID
			}
			info := runtime.SubagentInfo{SubagentID: task.SubagentID, TaskID: task.TaskID, Agent: task.Agent,
				Task: shortTask(task.Task), LastRunID: lastRunID, Status: runtime.RunStatus(task.Status)}
			if exists {
				info.CanFollowUp = items[index].CanFollowUp
			}
			if task.Status == "queued" || task.Status == "running" {
				info.CanFollowUp = false
			}
			if exists {
				items[index] = info
			} else {
				positions[task.SubagentID] = len(items)
				items = append(items, info)
			}
			updated[task.SubagentID] = task.UpdatedAt
		}
		sort.Slice(items, func(i, j int) bool {
			a, b := updated[items[i].SubagentID], updated[items[j].SubagentID]
			if a.Equal(b) {
				return items[i].SubagentID > items[j].SubagentID
			}
			return a.After(b)
		})
		if len(items) > 5 {
			items = items[:5]
		}
	}
	return items, nil
}

func (m *Manager) checkCheckpoint(ctx context.Context, row store.SubagentSession) error {
	key := fmt.Sprintf("%d/%s", row.ConversationID, row.ID)
	m.activeMu.Lock()
	blocked, exists := m.unsaved[key] // unsaved 是之前标记过 写入有问题的就标记成unsaved
	// 标记不同 说明被其他进程修改过 被处理 已经不是unsaved的了
	if exists && (blocked.Revision != row.Revision || blocked.RunID != row.LatestRunID) {
		delete(m.unsaved, key)
		exists = false
	}
	m.activeMu.Unlock()
	if exists { // 如果存在 且没有别人修改过 就说明一直是有问题
		return ErrCheckpointUnavailable
	}
	if verifier, ok := m.subagentSessionsStore.(CheckpointVerifier); ok {
		if err := verifier.CheckCheckpoint(ctx, row); err != nil {
			return err
		}
	}
	if m.background != nil {
		if verifier, ok := m.background.store.(TaskCheckpointVerifier); ok {
			return verifier.CheckTaskCheckpoint(ctx, row)
		}
	}
	return nil
}

func shortTask(text string) string {
	runes := []rune(text)
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return text
}
