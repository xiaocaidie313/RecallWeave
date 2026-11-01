package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/delivery"
	"recallweave/internal/store"
)

var _ runtime.GroupDelegator = (*Manager)(nil)

func (m *Manager) GroupsEnabled() bool {
	if !m.BackgroundEnabled() {
		return false
	}
	repository, ok := m.backgroundRunner.store.(GroupTaskStore)
	return ok && repository.GroupsEnabled()
}

func (m *Manager) groupRepository() (GroupTaskStore, error) {
	if !m.GroupsEnabled() {
		return nil, ErrBackgroundUnavailable
	}
	return m.backgroundRunner.store.(GroupTaskStore), nil
}

func (r *backgroundRunner) reserveGroup(ctx context.Context, count int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closing || r.ctx.Err() != nil {
		return ErrBackgroundUnavailable
	}
	if cap(r.slots)-len(r.slots) < count {
		return ErrBackgroundFull
	}
	for i := 0; i < count; i++ {
		r.slots <- struct{}{}
	}
	r.pending.Add(count)
	return nil
}

func boundedGroupText(value string, limit int, required bool) bool {
	return utf8.ValidString(value) && len(value) <= limit && (!required || strings.TrimSpace(value) != "")
}

func (m *Manager) SubmitGroup(ctx context.Context, parent runtime.DelegationParent, request runtime.GroupDelegationRequest) (runtime.TaskGroup, error) {
	repository, err := m.groupRepository()
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	if parent.Agent == nil || parent.Agent.Model == nil || parent.ConversationID == 0 || parent.RunID == "" {
		return runtime.TaskGroup{}, fmt.Errorf("subagent: a live scoped parent and conversation are required")
	}
	if len(request.Tasks) != 2 || !boundedGroupText(request.Goal, 8<<10, true) || !boundedGroupText(request.Context, 8<<10, false) || !boundedGroupText(request.ExpectedOutput, 2<<10, false) {
		return runtime.TaskGroup{}, fmt.Errorf("subagent: a group needs one bounded goal and exactly two tasks")
	}
	mode, err := delivery.NormalizeNewGroupMode(request.DeliveryMode)
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	presets := make([]PresetAgent, 2)
	inputs := make([]string, 2)
	for i, task := range request.Tasks {
		if !boundedGroupText(task.Task, 8<<10, true) || !boundedGroupText(task.Context, 8<<10, false) || !boundedGroupText(task.ExpectedOutput, 2<<10, false) {
			return runtime.TaskGroup{}, fmt.Errorf("subagent: invalid member task %d", i+1)
		}
		name := task.Agent
		if name == "" {
			name = m.defaultAgent
		}
		preset, ok := m.PresetAgents[name]
		if !ok {
			return runtime.TaskGroup{}, fmt.Errorf("subagent: unknown agent %q", name)
		}
		presets[i] = preset
		memberContext := strings.TrimSpace(request.Context + "\n" + task.Context)
		expected := strings.TrimSpace(request.ExpectedOutput + "\n" + task.ExpectedOutput)
		if len(memberContext) > 16<<10 || len(expected) > 4<<10 {
			return runtime.TaskGroup{}, fmt.Errorf("subagent: combined member instructions exceed the group budget")
		}
		inputs[i] = "共同目标（只负责本次分配的独立部分）：\n" + request.Goal + "\n\n" + taskInput(runtime.DelegationRequest{Task: task.Task, Context: memberContext, ExpectedOutput: expected})
	}
	r := m.backgroundRunner
	if err := r.reserveGroup(ctx, 2); err != nil {
		return runtime.TaskGroup{}, err
	}
	accepted := false
	jobs := make([]*backgroundJob, 0, 2)
	defer func() {
		if !accepted {
			for _, job := range jobs {
				job.cancel()
				job.release()
			}
			for i := 0; i < 2; i++ {
				<-r.slots
			}
		}
		r.pending.Done()
		r.pending.Done()
	}()
	groupID, err := newSubagentID()
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	now := time.Now()
	group := store.TaskGroup{ID: groupID, ConversationID: parent.ConversationID, ParentRunID: parent.RunID, Goal: strings.TrimSpace(request.Goal), ExpectedOutput: request.ExpectedOutput, DeliveryMode: mode, Status: "running", CreatedAt: now, UpdatedAt: now}
	rows := make([]store.SubagentTask, 0, 2)
	ids := make([]string, 0, 2)
	for i, task := range request.Tasks {
		id, err := newSubagentID()
		if err != nil {
			return runtime.TaskGroup{}, err
		}
		taskID, err := newSubagentID()
		if err != nil {
			return runtime.TaskGroup{}, err
		}
		release, err := m.lockSubagentSession(parent.ConversationID, id)
		if err != nil {
			return runtime.TaskGroup{}, err
		}
		jobCtx, cancel := context.WithCancel(r.ctx)
		var once sync.Once
		memberReportStatus := "grouped"
		if mode == delivery.Direct {
			memberReportStatus = ""
		}
		row := store.SubagentTask{ID: taskID, GroupID: groupID, SubagentID: id, ConversationID: parent.ConversationID, ParentRunID: parent.RunID, AgentName: presets[i].Name, Task: strings.TrimSpace(task.Task), DeliveryMode: delivery.Direct, Status: "queued", ReportStatus: memberReportStatus, CreatedAt: now, UpdatedAt: now}
		job := &backgroundJob{record: row, parent: runtime.DelegationParent{RunID: parent.RunID, ConversationID: parent.ConversationID}, child: m.newChild(parent.Agent, presets[i]), presetAgent: presets[i], input: inputs[i], ctx: jobCtx, cancel: cancel, release: func() { once.Do(release) }}
		jobs = append(jobs, job)
		rows = append(rows, row)
		ids = append(ids, taskID)
	}
	group.TaskIDs, _ = json.Marshal(ids)
	if err := repository.CreateGroup(ctx, &group, rows); err != nil {
		return runtime.TaskGroup{}, err
	}
	r.mu.Lock()
	for _, job := range jobs {
		r.jobs[job.record.ID] = job
	}
	r.mu.Unlock()
	for _, job := range jobs {
		r.queue <- job
	}
	accepted = true
	return m.groupView(group, rows)
}

func (m *Manager) groupView(group store.TaskGroup, members []store.SubagentTask) (runtime.TaskGroup, error) {
	mode, err := delivery.ResolveStoredGroupMode(group.DeliveryMode)
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	view := runtime.TaskGroup{GroupID: group.ID, ConversationID: group.ConversationID, ParentRunID: group.ParentRunID, Goal: group.Goal, ExpectedOutput: group.ExpectedOutput, Status: group.Status, CancelRequested: group.CancelRequested, ReportStatus: group.ReportStatus, ReportRunID: group.ReportRunID, ReportMessageID: group.ReportMessageID, ReportError: group.ReportError, ReportedAt: group.ReportedAt, CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt, FinishedAt: group.FinishedAt}
	view.DeliveryMode = mode
	if json.Unmarshal(group.TaskIDs, &view.TaskIDs) != nil || len(view.TaskIDs) != 2 || view.TaskIDs[0] == view.TaskIDs[1] {
		return view, fmt.Errorf("subagent: invalid stored group membership")
	}
	if members == nil {
		return view, nil
	}
	if len(members) != 2 {
		return view, fmt.Errorf("subagent: group membership is incomplete")
	}
	byID := make(map[string]store.SubagentTask, 2)
	for _, row := range members {
		if row.GroupID != group.ID || row.ConversationID != group.ConversationID || row.ParentRunID != group.ParentRunID {
			return view, fmt.Errorf("subagent: stored group ownership mismatch")
		}
		if _, exists := byID[row.ID]; exists {
			return view, fmt.Errorf("subagent: duplicate group member")
		}
		byID[row.ID] = row
	}
	for _, id := range view.TaskIDs {
		row, exists := byID[id]
		if !exists {
			return view, fmt.Errorf("subagent: group member unavailable")
		}
		m.backgroundRunner.mu.Lock()
		if failed, ok := m.backgroundRunner.failures[id]; ok && failed.ConversationID == group.ConversationID {
			row = failed
			view.Error = "部分子任务结果保存失败，任务组尚未完成持久化。"
		}
		m.backgroundRunner.mu.Unlock()
		item, err := m.taskView(row)
		if err != nil {
			return view, err
		}
		view.Tasks = append(view.Tasks, item)
	}
	return view, nil
}

func (m *Manager) GetGroup(ctx context.Context, parent runtime.DelegationParent, id string) (runtime.TaskGroup, error) {
	repository, err := m.groupRepository()
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	group, rows, err := repository.LoadGroup(ctx, id, parent.ConversationID)
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	if group.ID != id || group.ConversationID != parent.ConversationID {
		return runtime.TaskGroup{}, fmt.Errorf("subagent: stored group ownership mismatch")
	}
	return m.groupView(group, rows)
}

func (m *Manager) ListGroups(ctx context.Context, owner uint) ([]runtime.TaskGroup, error) {
	repository, err := m.groupRepository()
	if err != nil {
		return nil, err
	}
	rows, err := repository.ListGroups(ctx, owner, 10)
	if err != nil {
		return nil, err
	}
	items := make([]runtime.TaskGroup, 0, len(rows))
	for _, row := range rows {
		if row.ConversationID != owner {
			return nil, fmt.Errorf("subagent: stored group ownership mismatch")
		}
		view, err := m.groupView(row, nil)
		if err != nil {
			return nil, err
		}
		items = append(items, view)
	}
	return items, nil
}

func (m *Manager) CancelGroup(ctx context.Context, parent runtime.DelegationParent, id string) (runtime.TaskGroup, error) {
	repository, err := m.groupRepository()
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	group, rows, err := repository.RequestCancelGroup(ctx, id, parent.ConversationID)
	if err != nil {
		return runtime.TaskGroup{}, err
	}
	if group.ID != id || group.ConversationID != parent.ConversationID {
		return runtime.TaskGroup{}, fmt.Errorf("subagent: stored group ownership mismatch")
	}
	r := m.backgroundRunner
	r.mu.Lock()
	for _, row := range rows {
		if row.GroupID != id || row.ConversationID != parent.ConversationID {
			r.mu.Unlock()
			return runtime.TaskGroup{}, fmt.Errorf("subagent: stored member ownership mismatch")
		}
		if row.CancelRequested {
			if job := r.jobs[row.ID]; job != nil && job.record.ConversationID == parent.ConversationID {
				job.cancel()
				if row.Status == "cancelled" {
					job.release()
				}
			}
		}
	}
	r.mu.Unlock()
	return m.groupView(group, rows)
}
