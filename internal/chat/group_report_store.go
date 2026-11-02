package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"recallweave/internal/delivery"
	"recallweave/internal/events"
	"recallweave/internal/store"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ GroupReportStore = (*GormReportStore)(nil)

func (s *GormReportStore) PendingGroups(ctx context.Context, limit int) ([]store.TaskGroup, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows := make([]store.TaskGroup, 0)
	err := s.db.WithContext(ctx).Select("id", "conversation_id", "delivery_mode").Where("report_status = ? AND status IN ? AND COALESCE(delivery_mode, '') <> ?", "pending", []string{"completed", "partial", "failed", "cancelled", "interrupted"}, delivery.Direct).Order("finished_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s *GormReportStore) ClaimGroup(ctx context.Context, id string, owner uint, claim string) (store.TaskGroup, []store.SubagentTask, bool, error) {
	var group store.TaskGroup
	var tasks []store.SubagentTask
	if err := s.check(ctx); err != nil {
		return group, nil, false, err
	}
	if err := reportIdentity(id, owner, claim); err != nil {
		return group, nil, false, err
	}
	claimed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&store.TaskGroup{}).Where("id = ? AND conversation_id = ? AND report_status = ? AND status IN ? AND COALESCE(delivery_mode, '') <> ?", id, owner, "pending", []string{"completed", "partial", "failed", "cancelled", "interrupted"}, delivery.Direct).Updates(map[string]any{"report_status": "processing", "report_claim": claim, "report_error": "", "updated_at": tx.NowFunc()})
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		if err := tx.Where("id = ? AND conversation_id = ?", id, owner).Take(&group).Error; err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		ids, err := groupTaskIDs(group)
		if err != nil {
			return err
		}
		if err := tx.Where("id IN ? AND group_id = ? AND conversation_id = ?", ids, id, owner).Find(&tasks).Error; err != nil {
			return err
		}
		_, _, _, err = groupReportInputs(group, tasks)
		return err
	})
	if err != nil {
		return store.TaskGroup{}, nil, false, err
	}
	return group, tasks, claimed, nil
}

func (s *GormReportStore) DeliverGroup(ctx context.Context, id string, owner uint, claim string, report GeneratedReport) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := reportIdentity(id, owner, claim); err != nil {
		return err
	}
	if report.Message.Role != "assistant" || !validReportText(report.Message.Content) {
		return fmt.Errorf("chat: group report must contain a bounded assistant message")
	}
	if err := checkReportRun(report.Run, owner); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		var group store.TaskGroup
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&group).Error; err != nil {
			return err
		}
		if group.ReportStatus == "delivered" {
			return nil
		}
		if group.ReportStatus != "processing" || group.ReportClaim != claim || !reportTerminal(group.Status) {
			return ErrReportConflict
		}
		mode, err := delivery.ResolveStoredGroupMode(group.DeliveryMode)
		if err != nil {
			return err
		}
		if mode == delivery.Direct {
			return ErrReportConflict
		}
		if _, err := groupTaskIDs(group); err != nil {
			return err
		}
		var session store.Session
		err = tx.Where("conversation_id = ? AND source_tag = ?", owner, store.SourceChat).Order("id").First(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			session = store.Session{ConversationID: &owner, SourceTag: store.SourceChat, Name: "chat", ExtractStatus: store.ExtractPending}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := lockReportSession(tx, &session, owner); err != nil {
			return err
		}
		message := store.Message{SourceTag: store.SourceChat, SessionID: session.ID, Role: "assistant", Content: report.Message.Content}
		if err := appendReportMessage(tx, &message); err != nil {
			return err
		}
		runID := ""
		if report.Run != nil {
			runCopy := *report.Run
			if err := tx.Create(&runCopy).Error; err != nil {
				return err
			}
			runID = runCopy.RunID
		}
		now := tx.NowFunc()
		result := tx.Model(&store.TaskGroup{}).Where("id = ? AND conversation_id = ? AND report_status = ? AND report_claim = ?", id, owner, "processing", claim).Updates(map[string]any{"report_status": "delivered", "report_claim": "", "report_run_id": runID, "report_message_id": message.ID, "report_error": clipReportError(report.Error), "reported_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrReportConflict
		}
		return appendMessageEvent(tx, owner, message, messageEventMetadata{GroupID: id, RunID: runID, Sources: report.Sources})
	})
}

func (s *GormReportStore) FailGroup(ctx context.Context, id string, owner uint, claim, reason string, run *store.AgentRun) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := reportIdentity(id, owner, claim); err != nil {
		return err
	}
	if err := checkReportRun(run, owner); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("chat: group report failure reason is required")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		var group store.TaskGroup
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&group).Error; err != nil {
			return err
		}
		if group.ReportStatus == "delivered" || group.ReportStatus == "failed" {
			return nil
		}
		if group.ReportStatus != "processing" || group.ReportClaim != claim {
			return ErrReportConflict
		}
		runID := ""
		if run != nil {
			runCopy := *run
			if err := tx.Create(&runCopy).Error; err != nil {
				return err
			}
			runID = runCopy.RunID
		}
		now := tx.NowFunc()
		reason = clipReportError(reason)
		result := tx.Model(&store.TaskGroup{}).Where("id = ? AND conversation_id = ? AND report_status = ? AND report_claim = ?", id, owner, "processing", claim).Updates(map[string]any{"report_status": "failed", "report_claim": "", "report_run_id": runID, "report_error": reason, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrReportConflict
		}
		data, err := json.Marshal(map[string]any{"conversation_id": owner, "group_id": id, "run_id": runID, "error": reason})
		if err != nil {
			return err
		}
		return events.Append(tx, &store.ConversationEvent{ConversationID: owner, Kind: "report.failed", Key: "group-report:" + id + ":failed", Data: data})
	})
}

func (s *GormReportStore) InterruptGroupProcessing(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	var after string
	for {
		var rows []store.TaskGroup
		query := s.db.WithContext(ctx).Where("report_status = ?", "processing")
		if after != "" {
			query = query.Where("id > ?", after)
		}
		if err := query.Order("id").Limit(100).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, group := range rows {
			if err := s.recoverInterruptedReport(ctx, "task_groups", group.ID, group.ConversationID, group.ReportClaim); err != nil {
				return err
			}
			after = group.ID
		}
	}
}
