package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/delivery"
	"recallweave/internal/events"
	"recallweave/internal/types"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type GormTaskStore struct {
	db        *gorm.DB
	events    bool // 是否开启事件 sse
	groupMode bool // 是否开启组任务
}

func (s *GormTaskStore) WithTaskGroups() *GormTaskStore {
	if s != nil {
		s.groupMode = true
	}
	return s
}

func (s *GormTaskStore) GroupsEnabled() bool { return s != nil && s.groupMode }

func (s *GormTaskStore) WithEvents() *GormTaskStore {
	if s != nil {
		s.events = true
	}
	return s
}

func NewTaskStore(db *gorm.DB) *GormTaskStore {
	if db == nil {
		return nil
	}
	return &GormTaskStore{db: db}
}

var _ TaskStore = (*GormTaskStore)(nil)
var _ TaskCheckpointVerifier = (*GormTaskStore)(nil)

func (s *GormTaskStore) check(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("subagent: task storage context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil {
		return fmt.Errorf("subagent: task storage is unavailable")
	}
	return nil
}
func checkTaskIdentity(id string, owner uint) error {
	if owner == 0 || !validTaskID(id, false) {
		return fmt.Errorf("subagent: task id and owning conversation are required")
	}
	return nil
}

// 是否非空 / 长度<= 64 / 有无空格 / 是否合法utf8
func validTaskID(id string, optional bool) bool {
	return (id != "" || optional) && len(id) <= 64 && strings.TrimSpace(id) == id && utf8.ValidString(id)
}
func checkTaskRecord(record *types.SubagentTask) error {
	if record == nil {
		return fmt.Errorf("subagent: task record is required")
	}
	if err := checkTaskIdentity(record.ID, record.ConversationID); err != nil {
		return err
	}
	if !validTaskID(record.SubagentID, false) || !validTaskID(record.ParentRunID, false) || !validTaskID(record.RunID, true) || !validTaskID(record.PreviousRunID, true) || !validTaskID(record.GroupID, true) {
		return fmt.Errorf("subagent: task execution identities are invalid")
	}
	if strings.TrimSpace(record.AgentName) == "" || len(record.AgentName) > 40 || !utf8.ValidString(record.AgentName) || strings.TrimSpace(record.Task) == "" || len(record.Task) > 8<<10 || !utf8.ValidString(record.Task) {
		return fmt.Errorf("subagent: task role and bounded task text are required")
	}
	if len(record.Result) > 0 && !json.Valid(record.Result) {
		return fmt.Errorf("subagent: task result must be valid JSON")
	}
	if _, err := delivery.NormalizeTaskMode(record.DeliveryMode); err != nil {
		return err
	}
	return nil
}
func (s *GormTaskStore) Create(ctx context.Context, task *types.SubagentTask) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := checkTaskRecord(task); err != nil {
		return err
	}
	if task.GroupID != "" || task.Status != "queued" || task.RunID != "" || task.CancelRequested || len(task.Result) > 0 || task.StartedAt != nil || task.FinishedAt != nil || task.ReportStatus != "" || task.ReportClaim != "" || task.ReportRunID != "" || task.ReportMessageID != 0 || task.ReportError != "" || task.ReportedAt != nil {
		return fmt.Errorf("subagent: a new background task must be queued and unexecuted")
	}
	record := *task
	record.DeliveryMode, _ = delivery.NormalizeTaskMode(record.DeliveryMode)
	// create 可能会回填字段  保护原始的task字段
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	*task = record
	return nil
}
func (s *GormTaskStore) Load(ctx context.Context, id string, owner uint) (types.SubagentTask, error) {
	var record types.SubagentTask
	if err := s.check(ctx); err != nil {
		return record, err
	}
	if err := checkTaskIdentity(id, owner); err != nil {
		return record, err
	}
	if err := s.db.WithContext(ctx).Where("id = ? AND conversation_id = ? ", id, owner).Take(&record).Error; err != nil {
		return record, err
	}
	return record, nil
}
func (s *GormTaskStore) List(ctx context.Context, owner uint, limit int) ([]types.SubagentTask, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if owner == 0 {
		return nil, fmt.Errorf("subagent: owning conversation is required")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	records := make([]types.SubagentTask, 0)
	err := s.db.WithContext(ctx).Where("conversation_id = ?", owner).Order("created_at DESC").Order("id DESC").Limit(limit).Find(&records).Error
	return records, err
}

// 启动任务 更新库状态为 running
func (s *GormTaskStore) Start(ctx context.Context, id string, owner uint) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	if err := checkTaskIdentity(id, owner); err != nil {
		return false, err
	}
	now := s.db.NowFunc()
	result := s.db.WithContext(ctx).Model(&types.SubagentTask{}).Where("id = ? AND conversation_id = ? AND status = ? AND cancel_requested = ?", id, owner, "queued", false).Updates(map[string]any{"status": "running", "started_at": now, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	// 命中一行 id + conversation_id 同时约束
	if result.RowsAffected == 1 {
		return true, nil
	}
	// 查看是否能load
	if _, err := s.Load(ctx, id, owner); err != nil {
		return false, err
	}
	return false, nil
}
func taskTerminalStatus(status string) bool {
	switch status {
	case "completed", "partial", "failed", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}
func (s *GormTaskStore) Finish(ctx context.Context, task *types.SubagentTask, run *types.AgentRun) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := checkTaskRecord(task); err != nil {
		return err
	}
	// 是否是终止状态
	if !taskTerminalStatus(task.Status) {
		return fmt.Errorf("subagent: task completion requires a terminal status")
	}
	if run != nil && (task.RunID == "" || run.RunID != task.RunID || run.ConversationID != task.ConversationID || run.SubagentID != task.SubagentID || run.ParentRunID != task.ParentRunID || run.PreviousRunID != task.PreviousRunID || len(run.State) == 0 || !json.Valid(run.State)) {
		return fmt.Errorf("subagent: execution Run does not match this task")
	}
	record := *task
	if record.GroupID != "" && !s.GroupsEnabled() {
		return fmt.Errorf("subagent: task groups are unavailable")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.events || s.groupMode {
			// 锁住 conversation
			if err := events.LockConversation(tx, record.ConversationID); err != nil {
				return err
			}
		}
		var group *types.TaskGroup
		if record.GroupID != "" {
			var err error
			group, err = lockTaskGroup(tx, record.GroupID, record.ConversationID)
			if err != nil {
				return err
			}
		}
		var current types.SubagentTask
		//
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", record.ID, record.ConversationID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != "queued" && current.Status != "running" {
			return ErrTaskConflict
		}
		if current.GroupID != record.GroupID || current.SubagentID != record.SubagentID || current.ParentRunID != record.ParentRunID || current.PreviousRunID != record.PreviousRunID || current.AgentName != record.AgentName || current.Task != record.Task {
			return fmt.Errorf("subagent: completion cannot change task ownership or definition")
		}
		currentMode, err := delivery.NormalizeTaskMode(current.DeliveryMode)
		if err != nil {
			return err
		}
		recordMode, _ := delivery.NormalizeTaskMode(record.DeliveryMode)
		if currentMode != recordMode {
			return fmt.Errorf("subagent: completion cannot change delivery mode")
		}
		record.DeliveryMode = current.DeliveryMode
		now := tx.NowFunc()
		record.CreatedAt, record.StartedAt, record.CancelRequested = current.CreatedAt, current.StartedAt, current.CancelRequested
		record.UpdatedAt, record.FinishedAt = now, &now
		fields := []string{"run_id", "status", "result", "error", "finished_at", "updated_at"}
		if s.events || record.GroupID != "" {
			var err error
			record.ReportStatus, err = s.memberReportStatus(group)
			if err != nil {
				return err
			}
			fields = append(fields, "report_status")
		}
		result := tx.Model(&types.SubagentTask{}).Where("id = ? AND conversation_id = ? AND status = ?", record.ID, record.ConversationID, current.Status).Select(fields).Updates(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskConflict
		}
		if run != nil {
			runCopy := *run
			if err := tx.Create(&runCopy).Error; err != nil {
				return err
			}
		}
		if s.events {
			if err := appendTaskFinished(tx, record); err != nil {
				return err
			}
		}
		if group != nil {
			return s.refreshTaskGroup(tx, group)
		}
		return nil
	})
	if err == nil {
		*task = record
	}
	return err
}
func (s *GormTaskStore) RequestCancel(ctx context.Context, id string, owner uint) (types.SubagentTask, error) {
	var record types.SubagentTask
	if err := s.check(ctx); err != nil {
		return record, err
	}
	if err := checkTaskIdentity(id, owner); err != nil {
		return record, err
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.events || s.groupMode {
			if err := events.LockConversation(tx, owner); err != nil {
				return err
			}
		}
		var group *types.TaskGroup
		if s.groupMode {
			var err error
			group, err = lockTaskGroupForMember(tx, id, owner)
			if err != nil {
				return err
			}
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&record).Error; err != nil {
			return err
		}
		if record.GroupID != "" && (group == nil || group.ID != record.GroupID) {
			return fmt.Errorf("subagent: grouped cancellation requires its group lock")
		}
		if taskTerminalStatus(record.Status) || record.CancelRequested {
			return nil
		}
		if record.Status != "queued" && record.Status != "running" {
			return fmt.Errorf("subagent: invalid typesd task state %q", record.Status)
		}
		now := tx.NowFunc()
		oldStatus := record.Status
		record.CancelRequested, record.UpdatedAt = true, now
		fields := []string{"cancel_requested", "updated_at"}
		if record.Status == "queued" {
			record.Status, record.FinishedAt = "cancelled", &now
			fields = append(fields, "status", "finished_at")
			if s.events || record.GroupID != "" {
				var err error
				record.ReportStatus, err = s.memberReportStatus(group)
				if err != nil {
					return err
				}
				fields = append(fields, "report_status")
			}
		}
		result := tx.Model(&types.SubagentTask{}).Where("id = ? AND conversation_id = ? AND status = ?", id, owner, oldStatus).Select(fields).Updates(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskConflict
		}
		if s.events && record.Status == "cancelled" {
			if err := appendTaskFinished(tx, record); err != nil {
				return err
			}
		}
		if group != nil {
			return s.refreshTaskGroup(tx, group)
		}
		return nil
	})
	if err != nil {
		return types.SubagentTask{}, err
	}
	return record, nil
}
func (s *GormTaskStore) InterruptUnfinished(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if s.events || s.groupMode {
		return s.interruptWithEvents(ctx)
	}
	now := s.db.NowFunc()
	return s.db.WithContext(ctx).Model(&types.SubagentTask{}).Where("status IN ?", []string{"queued", "running"}).Updates(map[string]any{"status": "interrupted", "error": "server stopped before task finished; automatic replay is disabled", "finished_at": now, "updated_at": now}).Error
}
func (s *GormTaskStore) CheckTaskCheckpoint(ctx context.Context, session types.SubagentSession) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := checkTaskIdentity(session.ID, session.ConversationID); err != nil {
		return err
	}
	if !validTaskID(session.LatestRunID, false) {
		return fmt.Errorf("subagent: checkpoint run id is required")
	}
	var interrupted int64
	// 检查当前session 在 某一子agent 某一会话 开始在前一次会话后续上， 且已经开始了 但是 又中断了
	err := s.db.WithContext(ctx).Model(&types.SubagentTask{}).Where("subagent_id = ? AND conversation_id = ? AND previous_run_id = ? AND status = ? AND started_at IS NOT NULL", session.ID, session.ConversationID, session.LatestRunID, "interrupted").Count(&interrupted).Error
	if err != nil {
		return err
	}
	// 如果有中断状态 说明前一次运行的后续运行有中断 不能继续
	if interrupted > 0 {
		return ErrCheckpointUnavailable
	}
	return nil
}
