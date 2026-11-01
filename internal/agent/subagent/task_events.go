package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"recallweave/internal/events"
	"recallweave/internal/store"
)

func appendTaskFinished(tx *gorm.DB, task store.SubagentTask) error {
	data, err := json.Marshal(struct {
		ConversationID uint   `json:"conversation_id"`
		TaskID         string `json:"task_id"`
		GroupID        string `json:"group_id,omitempty"`
		SubagentID     string `json:"subagent_id"`
		RunID          string `json:"run_id,omitempty"`
		Status         string `json:"status"`
		Error          string `json:"error,omitempty"`
	}{task.ConversationID, task.ID, task.GroupID, task.SubagentID, task.RunID, task.Status, task.Error})
	if err != nil {
		return err
	}
	return events.Append(tx, &store.ConversationEvent{ConversationID: task.ConversationID, Kind: "task.finished", Key: "task:" + task.ID + ":finished", Data: data})
}

func (s *GormTaskStore) interruptWithEvents(ctx context.Context) error {
	for {
		var batch []store.SubagentTask
		if err := s.db.WithContext(ctx).Where("status IN ?", []string{"queued", "running"}).Order("id").Limit(100).Find(&batch).Error; err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, item := range batch {
			if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := events.LockConversation(tx, item.ConversationID); err != nil {
					return err
				}
				var group *store.TaskGroup
				if item.GroupID != "" {
					if !s.groupMode {
						return fmt.Errorf("subagent: task groups are unavailable during recovery")
					}
					var err error
					group, err = lockTaskGroup(tx, item.GroupID, item.ConversationID)
					if err != nil {
						return err
					}
				}
				var current store.SubagentTask
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", item.ID, item.ConversationID).Take(&current).Error; err != nil {
					return err
				}
				if current.GroupID != item.GroupID {
					return fmt.Errorf("subagent: group membership changed during recovery")
				}
				if current.Status != "queued" && current.Status != "running" {
					return nil
				}
				now := tx.NowFunc()
				current.Status = "interrupted"
				current.Error = "server stopped before task finished; automatic replay is disabled"
				var err error
				current.ReportStatus, err = s.memberReportStatus(group)
				if err != nil {
					return err
				}
				current.FinishedAt = &now
				current.UpdatedAt = now
				if err := tx.Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ?", current.ID, current.ConversationID).Select("status", "error", "report_status", "finished_at", "updated_at").Updates(&current).Error; err != nil {
					return err
				}
				if s.events {
					if err := appendTaskFinished(tx, current); err != nil {
						return err
					}
				}
				if group != nil {
					return s.refreshTaskGroup(tx, group)
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
}
