package chat

import (
	"context"
	"encoding/json"
	"fmt"

	"recallweave/internal/delivery"
	"recallweave/internal/events"
	"recallweave/internal/store"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type interruptedReport struct {
	ID              string
	ConversationID  uint
	DeliveryMode    string
	Status          string
	ReportStatus    string
	ReportClaim     string
	ReportRunID     string
	ReportMessageID uint
}

// Startup recovery only retries deterministic rendering. A synthesis claim may
// already have called its model, so it must not be replayed automatically.
func (s *GormReportStore) recoverInterruptedReport(ctx context.Context, table, id string, owner uint, claim string) error {
	if table != "subagent_tasks" && table != "task_groups" {
		return fmt.Errorf("chat: unknown report recovery table")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		var current interruptedReport
		if err := tx.Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&current).Error; err != nil {
			return err
		}
		// The startup scan is only a snapshot. Never undo a committed delivery,
		// another recovery, or a different claim that appeared after that scan.
		if current.ReportStatus != "processing" || current.ReportClaim != claim {
			return nil
		}
		var mode string
		var modeErr error
		if table == "task_groups" {
			mode, modeErr = delivery.ResolveStoredGroupMode(current.DeliveryMode)
		} else {
			mode, modeErr = delivery.NormalizeTaskMode(current.DeliveryMode)
		}
		retry := modeErr == nil && reportTerminal(current.Status) && current.ReportRunID == "" && current.ReportMessageID == 0 && (table == "subagent_tasks" && mode == delivery.Direct || table == "task_groups" && mode == delivery.Combined)
		fields := map[string]any{"report_claim": "", "updated_at": tx.NowFunc()}
		if retry {
			fields["report_status"], fields["report_error"] = "pending", ""
		} else {
			fields["report_status"] = "failed"
			fields["report_error"] = "automatic report was interrupted; model generation is not replayed automatically"
			if modeErr != nil {
				fields["report_error"] = "automatic report was interrupted with an invalid delivery mode"
			} else if mode != delivery.Synthesize {
				fields["report_error"] = "automatic report was interrupted with inconsistent delivery state"
			}
		}
		updated := tx.Table(table).Where("id = ? AND conversation_id = ? AND report_status = ? AND report_claim = ?", id, owner, "processing", claim).Updates(fields)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrReportConflict
		}
		if retry {
			return nil
		}
		key, reference := "report:", "task_id"
		if table == "task_groups" {
			key, reference = "group-report:", "group_id"
		}
		data, err := json.Marshal(map[string]any{"conversation_id": owner, reference: id, "run_id": current.ReportRunID, "error": fields["report_error"]})
		if err != nil {
			return err
		}
		return events.Append(tx, &store.ConversationEvent{ConversationID: owner, Kind: "report.failed", Key: key + id + ":failed", Data: data})
	})
}
