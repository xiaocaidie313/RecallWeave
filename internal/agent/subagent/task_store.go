package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/store"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type GormTaskStore struct {
	db *gorm.DB
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
func validTaskID(id string, optional bool) bool {
	return (id != "" || optional) && len(id) <= 64 && strings.TrimSpace(id) == id && utf8.ValidString(id)
}
func checkTaskRecord(record *store.SubagentTask) error {
	if record == nil {
		return fmt.Errorf("subagent: task record is required")
	}
	if err := checkTaskIdentity(record.ID, record.ConversationID); err != nil {
		return err
	}
	if !validTaskID(record.SubagentID, false) || !validTaskID(record.ParentRunID, false) || !validTaskID(record.RunID, true) || !validTaskID(record.PreviousRunID, true) {
		return fmt.Errorf("subagent: task execution identities are invalid")
	}
	if strings.TrimSpace(record.AgentName) == "" || len(record.AgentName) > 40 || !utf8.ValidString(record.AgentName) || strings.TrimSpace(record.Task) == "" || len(record.Task) > 8<<10 || !utf8.ValidString(record.Task) {
		return fmt.Errorf("subagent: task role and bounded task text are required")
	}
	if len(record.Result) > 0 && !json.Valid(record.Result) {
		return fmt.Errorf("subagent: task result must be valid JSON")
	}
	return nil
}
func (s *GormTaskStore) Create(ctx context.Context, task *store.SubagentTask) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := checkTaskRecord(task); err != nil {
		return err
	}
	if task.Status != "queued" || task.RunID != "" || task.CancelRequested || len(task.Result) > 0 || task.StartedAt != nil || task.FinishedAt != nil {
		return fmt.Errorf("subagent: a new background task must be queued and unexecuted")
	}
	record := *task
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	*task = record
	return nil
}
func (s *GormTaskStore) Load(ctx context.Context, id string, owner uint) (store.SubagentTask, error) {
	var record store.SubagentTask
	if err := s.check(ctx); err != nil {
		return record, err
	}
	if err := checkTaskIdentity(id, owner); err != nil {
		return record, err
	}
	err := s.db.WithContext(ctx).Where("id = ? AND conversation_id = ?", id, owner).Take(&record).Error
	return record, err
}
func (s *GormTaskStore) List(ctx context.Context, owner uint, limit int) ([]store.SubagentTask, error) {
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
	records := make([]store.SubagentTask, 0)
	err := s.db.WithContext(ctx).Where("conversation_id = ?", owner).Order("created_at DESC").Order("id DESC").Limit(limit).Find(&records).Error
	return records, err
}
func (s *GormTaskStore) Start(ctx context.Context, id string, owner uint) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	if err := checkTaskIdentity(id, owner); err != nil {
		return false, err
	}
	now := s.db.NowFunc()
	result := s.db.WithContext(ctx).Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ? AND status = ? AND cancel_requested = ?", id, owner, "queued", false).Updates(map[string]any{"status": "running", "started_at": now, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		return true, nil
	}
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
func (s *GormTaskStore) Finish(ctx context.Context, task *store.SubagentTask, run *store.AgentRun) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := checkTaskRecord(task); err != nil {
		return err
	}
	if !taskTerminalStatus(task.Status) {
		return fmt.Errorf("subagent: task completion requires a terminal status")
	}
	if run != nil && (task.RunID == "" || run.RunID != task.RunID || run.ConversationID != task.ConversationID || run.SubagentID != task.SubagentID || run.ParentRunID != task.ParentRunID || run.PreviousRunID != task.PreviousRunID || len(run.State) == 0 || !json.Valid(run.State)) {
		return fmt.Errorf("subagent: execution Run does not match this task")
	}
	record := *task
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current store.SubagentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", record.ID, record.ConversationID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != "queued" && current.Status != "running" {
			return ErrTaskConflict
		}
		if current.SubagentID != record.SubagentID || current.ParentRunID != record.ParentRunID || current.PreviousRunID != record.PreviousRunID || current.AgentName != record.AgentName || current.Task != record.Task {
			return fmt.Errorf("subagent: completion cannot change task ownership or definition")
		}
		now := tx.NowFunc()
		record.CreatedAt, record.StartedAt, record.CancelRequested = current.CreatedAt, current.StartedAt, current.CancelRequested
		record.UpdatedAt, record.FinishedAt = now, &now
		result := tx.Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ? AND status = ?", record.ID, record.ConversationID, current.Status).Select("run_id", "status", "result", "error", "finished_at", "updated_at").Updates(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskConflict
		}
		if run != nil {
			runCopy := *run
			return tx.Create(&runCopy).Error
		}
		return nil
	})
	if err == nil {
		*task = record
	}
	return err
}
func (s *GormTaskStore) RequestCancel(ctx context.Context, id string, owner uint) (store.SubagentTask, error) {
	var record store.SubagentTask
	if err := s.check(ctx); err != nil {
		return record, err
	}
	if err := checkTaskIdentity(id, owner); err != nil {
		return record, err
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&record).Error; err != nil {
			return err
		}
		if taskTerminalStatus(record.Status) || record.CancelRequested {
			return nil
		}
		if record.Status != "queued" && record.Status != "running" {
			return fmt.Errorf("subagent: invalid stored task state %q", record.Status)
		}
		now := tx.NowFunc()
		oldStatus := record.Status
		record.CancelRequested, record.UpdatedAt = true, now
		fields := []string{"cancel_requested", "updated_at"}
		if record.Status == "queued" {
			record.Status, record.FinishedAt = "cancelled", &now
			fields = append(fields, "status", "finished_at")
		}
		result := tx.Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ? AND status = ?", id, owner, oldStatus).Select(fields).Updates(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskConflict
		}
		return nil
	})
	if err != nil {
		return store.SubagentTask{}, err
	}
	return record, nil
}
func (s *GormTaskStore) InterruptUnfinished(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	now := s.db.NowFunc()
	return s.db.WithContext(ctx).Model(&store.SubagentTask{}).Where("status IN ?", []string{"queued", "running"}).Updates(map[string]any{"status": "interrupted", "error": "server stopped before task finished; automatic replay is disabled", "finished_at": now, "updated_at": now}).Error
}
func (s *GormTaskStore) CheckTaskCheckpoint(ctx context.Context, session store.SubagentSession) error {
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
	err := s.db.WithContext(ctx).Model(&store.SubagentTask{}).Where("subagent_id = ? AND conversation_id = ? AND previous_run_id = ? AND status = ? AND started_at IS NOT NULL", session.ID, session.ConversationID, session.LatestRunID, "interrupted").Count(&interrupted).Error
	if err != nil {
		return err
	}
	if interrupted > 0 {
		return ErrCheckpointUnavailable
	}
	return nil
}
