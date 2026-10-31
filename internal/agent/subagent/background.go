package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/store"
	"recallweave/internal/watch/useage"
)

var ErrBackgroundFull = errors.New("subagent: background capacity is full; try again later")
var ErrBackgroundUnavailable = errors.New("subagent: background execution is unavailable")

type BackgroundConfig struct {
	Workers       int
	QueueCapacity int
}

type backgroundRunner struct {
	ctx      context.Context // 上下文
	cancel   context.CancelFunc
	store    TaskStore           // 任务存储
	queue    chan *backgroundJob // 队列
	slots    chan struct{}       // 占位符
	mu       sync.Mutex
	closing  bool
	jobs     map[string]*backgroundJob     // 工人实体正在执行的任务
	failures map[string]store.SubagentTask // 失败任务
	pending  sync.WaitGroup                // 排队情况
	workers  sync.WaitGroup                // 工人
	done     chan struct{}
}

type backgroundJob struct {
	record      store.SubagentTask       // 子agent 任务记录
	parent      runtime.DelegationParent // 父亲
	child       *agent.Agent             // 子agent
	presetAgent PresetAgent              // 预设agent
	input       string                   // 输入
	previous    *store.SubagentSession   // 前一个任务
	ctx         context.Context
	cancel      context.CancelFunc // 取消
	release     func()             // 释放
}

var _ runtime.BackgroundDelegator = (*Manager)(nil)

// StartBackground is assembled once before serving requests. Pending records
// belong to this single service process and are interrupted, never replayed.
func (m *Manager) StartBackground(ctx context.Context, repository TaskStore, config BackgroundConfig) error {
	if m == nil || !m.ContinuationsEnabled() || repository == nil || ctx == nil {
		return ErrBackgroundUnavailable
	}
	if m.backgroundRunner != nil {
		return fmt.Errorf("subagent: background runner already configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if config.Workers == 0 {
		config.Workers = 2
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = 8
	}
	// 工人 2-15   排队2-127
	if config.Workers < 1 || config.Workers > 16 || config.QueueCapacity < 1 || config.QueueCapacity > 128 {
		return fmt.Errorf("subagent: workers must be 1..16 and queue capacity 1..128")
	}
	if err := repository.InterruptUnfinished(ctx); err != nil {
		return fmt.Errorf("subagent: recover background task states: %w", err)
	}
	runnerCtx, cancel := context.WithCancel(ctx)
	// 总容量
	capacity := config.Workers + config.QueueCapacity
	r := &backgroundRunner{ctx: runnerCtx, cancel: cancel, store: repository,
		queue: make(chan *backgroundJob, capacity),
		slots: make(chan struct{}, capacity), // 占位符容量 和queue 容量一样
		jobs:  make(map[string]*backgroundJob), failures: make(map[string]store.SubagentTask), done: make(chan struct{})}
	m.backgroundRunner = r
	for i := 0; i < config.Workers; i++ {
		r.workers.Add(1)
		go m.backgroundWorkerWorkingJob(r)
	}
	//  开始的时候就一直等待关系
	go func() {
		<-r.ctx.Done()
		r.stopAdmission()
		r.pending.Wait() // 等待所有排队任务完成
		r.workers.Wait() // 等待所有工人完成
		close(r.done)
	}()
	return nil
}

func (m *Manager) BackgroundEnabled() bool { return m != nil && m.backgroundRunner != nil }

func (r *backgroundRunner) stopAdmission() {
	r.mu.Lock()
	r.closing = true
	r.cancel()
	r.mu.Unlock()

}

// CloseBackground waits for workers before their model/tool dependencies close.
func (m *Manager) CloseBackground(ctx context.Context) error {
	// 外部 新的 ctx 控制关闭
	if !m.BackgroundEnabled() {
		return nil
	}
	r := m.backgroundRunner
	r.stopAdmission()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// 预约占位
func (r *backgroundRunner) reserve(ctx context.Context) error {
	// 加锁 读 r.closing
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closing || r.ctx.Err() != nil {
		return ErrBackgroundUnavailable
	}

	select {
	case r.slots <- struct{}{}:
		r.pending.Add(1)
		return nil
	default:
		return ErrBackgroundFull
	}
}

func (m *Manager) Submit(ctx context.Context, parent runtime.DelegationParent, request runtime.DelegationRequest) (runtime.BackgroundTask, error) {
	if !m.BackgroundEnabled() || parent.Agent == nil || parent.Agent.Model == nil || parent.RunID == "" || parent.ConversationID == 0 {
		return runtime.BackgroundTask{}, ErrBackgroundUnavailable
	}
	if strings.TrimSpace(request.Task) == "" || len(request.Task) > 8<<10 || len(request.Context) > 16<<10 || len(request.ExpectedOutput) > 2<<10 || !utf8.ValidString(taskInput(request)) {
		return runtime.BackgroundTask{}, fmt.Errorf("subagent: invalid background task")
	}
	name := request.Agent
	if name == "" {
		name = m.defaultAgent
	}
	PresetAgentConfig, ok := m.PresetAgents[name]
	if !ok {
		return runtime.BackgroundTask{}, fmt.Errorf("subagent: unknown agent %q", name)
	}
	// 预约占位
	if err := m.backgroundRunner.reserve(ctx); err != nil {
		return runtime.BackgroundTask{}, err
	}
	id, err := newSubagentID()
	if err != nil {
		m.abortReservation()
		return runtime.BackgroundTask{}, err
	}
	// 锁定子任务
	release, err := m.lockSubagentSession(parent.ConversationID, id)
	if err != nil {
		m.abortReservation()
		return runtime.BackgroundTask{}, err
	}
	return m.enqueue(ctx, parent, taskInput(request), request.Task, PresetAgentConfig, id, nil, release)
}

func (m *Manager) SubmitFollowUp(ctx context.Context, parent runtime.DelegationParent, request runtime.FollowupRequest) (runtime.BackgroundTask, error) {
	if !m.BackgroundEnabled() {
		return runtime.BackgroundTask{}, ErrBackgroundUnavailable
	}
	if err := m.backgroundRunner.reserve(ctx); err != nil {
		return runtime.BackgroundTask{}, err
	}
	previous, PresetAgent, release, err := m.prepareFollowUp(ctx, parent, request)
	if err != nil {
		m.abortReservation()
		return runtime.BackgroundTask{}, err
	}
	return m.enqueue(ctx, parent, "继续此子任务：\n"+request.Message, previous.Task, PresetAgent, previous.ID, &previous, release)
}

func (m *Manager) abortReservation() {
	// 释放占位
	<-m.backgroundRunner.slots
	m.backgroundRunner.pending.Done()
}

func (m *Manager) enqueue(ctx context.Context, parent runtime.DelegationParent, input, task string, PresetAgentConfig PresetAgent, id string, previous *store.SubagentSession, release func()) (runtime.BackgroundTask, error) {
	r := m.backgroundRunner
	accepted := false
	defer func() {
		// 完成一次入队
		r.pending.Done()
		if !accepted {
			<-r.slots
			release() // 释放子任务  没有被接受 干不了就放掉
		}
	}()
	taskID, err := newSubagentID()
	if err != nil {
		return runtime.BackgroundTask{}, err
	}
	now := time.Now()
	record := store.SubagentTask{ID: taskID, SubagentID: id, ParentRunID: parent.RunID,
		ConversationID: parent.ConversationID, AgentName: PresetAgentConfig.Name, Task: task, Status: "queued", CreatedAt: now, UpdatedAt: now}
	if previous != nil {
		record.PreviousRunID = previous.LatestRunID
	}
	// Freeze the scoped tools before the parent Run returns. The child never
	// receives the parent's mutable RunState, history, or preloaded memories.
	child := m.newChild(parent.Agent, PresetAgentConfig)
	if err := r.store.Create(ctx, &record); err != nil {
		return runtime.BackgroundTask{}, err
	}
	// 继承runner的 ctx
	jobCtx, cancel := context.WithCancel(r.ctx)
	// 只执行一次 release
	var released sync.Once
	job := &backgroundJob{record: record, parent: runtime.DelegationParent{RunID: parent.RunID, ConversationID: parent.ConversationID},
		child: child, presetAgent: PresetAgentConfig, input: input, previous: previous, ctx: jobCtx, cancel: cancel, release: func() { released.Do(release) }}

	r.mu.Lock()
	r.jobs[record.ID] = nil
	r.mu.Unlock()
	// Every accepted submission owns a slot, so this buffered send cannot
	// wait for a worker. The channel stays open throughout shutdown.
	r.queue <- job
	accepted = true
	return m.taskView(record)
}

func (m *Manager) backgroundWorkerWorkingJob(r *backgroundRunner) {
	defer r.workers.Done()
	for {
		select {
		case job := <-r.queue:
			m.runBackgroundJob(r, job)
		case <-r.ctx.Done():
			r.stopAdmission() // 停止接收新任务
			r.pending.Wait()
			// 完成剩下任务
			for {
				select {
				case job := <-r.queue:
					m.runBackgroundJob(r, job)
				default:
					return
				}
			}
		}
	}
}

func (m *Manager) runBackgroundJob(r *backgroundRunner, job *backgroundJob) {
	defer func() {
		job.cancel()
		job.release()
		r.mu.Lock()
		delete(r.jobs, job.record.ID)
		r.mu.Unlock()
		<-r.slots
	}()
	// A panic from a tool must not take down the HTTP service or leave the
	// execution lock held. Its partially observed context is not replayed.
	defer func() {
		// 恢复 panic 值  控制不再往上炸
		if recovered := recover(); recovered != nil {
			row := job.record
			row.Status, row.Error = "interrupted", "subagent execution panicked; context cannot be safely replayed"
			now := time.Now()
			row.StartedAt = &now
			m.finishBackground(r, row, runtime.RunResult{})
			slog.Error("background subagent panicked", "task_id", row.ID)
		}
	}()
	// 单独写库
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	started, err := r.store.Start(ctx, job.record.ID, job.record.ConversationID)
	cancel()
	if err != nil {
		var row store.SubagentTask
		row = job.record
		row.Status, row.Error = "failed", "could not start task: "+err.Error()
		m.finishBackground(r, row, runtime.RunResult{})
		return
	}
	if !started {
		return
	} // A queued cancellation already has a terminal row.
	now := time.Now()
	job.record.StartedAt = &now
	// 因为 后台 或者 单独任务 ctx 被取消了，所以需要返回
	if r.ctx.Err() != nil || job.ctx.Err() != nil {
		// 因为 任务的 ctx 被取消
		row := job.record
		row.Status, row.Error = "cancelled", "task cancelled before execution"
		// 因为 runner ctx 被取消了，所以需要返回
		if r.ctx.Err() != nil {
			row.Status, row.Error = "interrupted", "service stopped before task execution"
		}
		m.finishBackground(r, row, runtime.RunResult{})
		return
	}
	// 执行子任务
	result, runErr := m.executeChildSession(job.ctx, job.child, job.parent, job.input, job.record.Task, job.presetAgent, job.record.SubagentID, job.previous)
	row := job.record
	row.RunID, row.Status = result.State.RunID, string(result.State.Status)
	if row.Status == "" {
		row.Status = "failed"
	}
	if runErr != nil {
		row.Error = clipError(runErr.Error())
	}
	if r.ctx.Err() != nil && row.Status == "cancelled" {
		row.Status = "interrupted"
	}
	m.finishBackground(r, row, result)
}

func clipError(value string) string {
	if len(value) <= 2048 {
		return value
	}
	cut := 2048
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// 完成整个后台任务--收尾
func (m *Manager) finishBackground(r *backgroundRunner, row store.SubagentTask, result runtime.RunResult) {
	var run *store.AgentRun
	if result.State.RunID != "" {
		var err error
		row.Result, err = json.Marshal(result)
		if err != nil {
			row.Status, row.Error = "failed", "could not encode task result: "+err.Error()
		} else {
			state, stateErr := json.Marshal(result.State)
			if stateErr == nil {
				total := useage.Sum(result.State.AllCalls())
				run = &store.AgentRun{RunID: result.State.RunID, ParentRunID: result.State.ParentRunID,
					SubagentID: result.State.SubagentID, PreviousRunID: result.State.PreviousRunID, ConversationID: result.State.ConversationID,
					Status: string(result.State.Status), Input: result.State.Input, Content: result.Content, State: state,
					PromptTokens: total.Usage.PromptTokens, CompletionTokens: total.Usage.CompletionTokens,
					LatencyMs: total.LatencyMs, Rounds: result.State.CurrentRound}
			}
		}
	}
	now := time.Now()
	row.FinishedAt, row.UpdatedAt = &now, now
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := r.store.Finish(ctx, &row, run)
	cancel()
	if err == nil || errors.Is(err, ErrTaskConflict) {
		return
	}
	// 写库失败，标记失败任务
	row.Status = "failed"
	row.Error = clipError("task result was not persisted: " + err.Error())
	r.mu.Lock()
	if len(r.failures) >= 128 {
		for key := range r.failures {
			delete(r.failures, key)
			break
		}
	}
	// 标记失败任务
	r.failures[row.ID] = row
	r.mu.Unlock()
	slog.Error("save background subagent result failed", "task_id", row.ID, "run_id", row.RunID, "error", err)
}

// 将 委派任务格式 转化成 后台任务视图
func (m *Manager) taskView(row store.SubagentTask) (runtime.BackgroundTask, error) {
	view := runtime.BackgroundTask{TaskID: row.ID, SubagentID: row.SubagentID, ParentRunID: row.ParentRunID,
		RunID: row.RunID, PreviousRunID: row.PreviousRunID, ConversationID: row.ConversationID,
		Agent: row.AgentName, Task: row.Task, Status: row.Status, CancelRequested: row.CancelRequested,
		Error: row.Error, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt}
	view.ReportStatus, view.ReportRunID, view.ReportMessageID = row.ReportStatus, row.ReportRunID, row.ReportMessageID
	view.ReportError, view.ReportedAt = row.ReportError, row.ReportedAt
	if len(row.Result) != 0 && string(row.Result) != "null" {
		var result runtime.RunResult
		if err := json.Unmarshal(row.Result, &result); err != nil {
			return view, fmt.Errorf("subagent: invalid stored task result")
		}
		var runErr error
		if row.Error != "" {
			runErr = errors.New(row.Error)
		}
		summary := runtime.SummarizeDelegation(result, runErr)
		m.activeMu.Lock()
		busy := m.active[fmt.Sprintf("%d/%s", row.ConversationID, row.SubagentID)]
		m.activeMu.Unlock()
		if busy {
			summary.CanFollowUp = false
		}
		view.Result = &summary
	}
	return view, nil
}

func (m *Manager) GetTask(ctx context.Context, parent runtime.DelegationParent, taskID string) (runtime.BackgroundTask, error) {
	if !m.BackgroundEnabled() {
		return runtime.BackgroundTask{}, ErrBackgroundUnavailable
	}
	row, err := m.backgroundRunner.store.Load(ctx, taskID, parent.ConversationID)
	if err != nil {
		return runtime.BackgroundTask{}, err
	}
	if row.ID != taskID || row.ConversationID != parent.ConversationID {
		return runtime.BackgroundTask{}, fmt.Errorf("subagent: stored task ownership mismatch")
	}
	m.backgroundRunner.mu.Lock()
	if failed, ok := m.backgroundRunner.failures[taskID]; ok && failed.ConversationID == parent.ConversationID {
		row = failed
	}
	m.backgroundRunner.mu.Unlock()
	return m.taskView(row)
}

// 列出任务
func (m *Manager) ListTasks(ctx context.Context, conversationID uint) ([]runtime.BackgroundTask, error) {
	if !m.BackgroundEnabled() {
		return nil, ErrBackgroundUnavailable
	}
	rows, err := m.backgroundRunner.store.List(ctx, conversationID, 20)
	if err != nil {
		return nil, err
	}
	items := make([]runtime.BackgroundTask, 0, len(rows))
	for _, row := range rows {
		if row.ConversationID != conversationID {
			return nil, fmt.Errorf("subagent: stored task ownership mismatch")
		}
		m.backgroundRunner.mu.Lock()
		if failed, ok := m.backgroundRunner.failures[row.ID]; ok {
			row = failed
		}
		m.backgroundRunner.mu.Unlock()
		view, err := m.taskView(row)
		if err != nil {
			return nil, err
		}
		items = append(items, view)
	}
	return items, nil
}

func (m *Manager) CancelTask(ctx context.Context, parent runtime.DelegationParent, taskID string) (runtime.BackgroundTask, error) {
	if !m.BackgroundEnabled() {
		return runtime.BackgroundTask{}, ErrBackgroundUnavailable
	}
	row, err := m.backgroundRunner.store.RequestCancel(ctx, taskID, parent.ConversationID)
	if err != nil {
		return runtime.BackgroundTask{}, err
	}
	if row.ID != taskID || row.ConversationID != parent.ConversationID {
		return runtime.BackgroundTask{}, fmt.Errorf("subagent: stored task ownership mismatch")
	}
	if row.CancelRequested {
		m.backgroundRunner.mu.Lock()
		if job := m.backgroundRunner.jobs[taskID]; job != nil && job.record.ConversationID == parent.ConversationID {
			job.cancel()
			if row.Status == "cancelled" {
				job.release()
			}
		}
		m.backgroundRunner.mu.Unlock()
	}
	return m.taskView(row)
}
