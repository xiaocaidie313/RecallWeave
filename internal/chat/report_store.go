package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/events"
	"recallweave/internal/store"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrReportConflict = errors.New("chat: report is not claimed by this execution")

type GormReportStore struct{ db *gorm.DB }

func NewReportStore(db *gorm.DB) *GormReportStore {
	if db == nil {
		return nil
	}
	return &GormReportStore{db: db}
}

var _ ReportStore = (*GormReportStore)(nil)
var _ TurnWriter = (*GormReportStore)(nil)

func (s *GormReportStore) check(ctx context.Context) error {
	if ctx == nil || s == nil || s.db == nil {
		return fmt.Errorf("chat: report storage and context are required")
	}
	return ctx.Err()
}
func reportIdentity(id string, owner uint, claim string) error {
	if owner == 0 || strings.TrimSpace(id) == "" || len(id) > 64 || strings.TrimSpace(id) != id || !utf8.ValidString(id) || strings.TrimSpace(claim) == "" || len(claim) > 64 || strings.TrimSpace(claim) != claim || !utf8.ValidString(claim) {
		return fmt.Errorf("chat: task, owner and report claim are required")
	}
	return nil
}
func reportTerminal(status string) bool {
	switch status {
	case "completed", "partial", "failed", "cancelled", "interrupted":
		return true
	}
	return false
}
func (s *GormReportStore) Pending(ctx context.Context, limit int) ([]store.SubagentTask, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows := make([]store.SubagentTask, 0)
	err := s.db.WithContext(ctx).Select("id", "conversation_id").Where("report_status = ? AND status IN ?", "pending", []string{"completed", "partial", "failed", "cancelled", "interrupted"}).Order("finished_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}
func (s *GormReportStore) Claim(ctx context.Context, id string, owner uint, claim string) (store.SubagentTask, bool, error) {
	var row store.SubagentTask
	if err := s.check(ctx); err != nil {
		return row, false, err
	}
	if err := reportIdentity(id, owner, claim); err != nil {
		return row, false, err
	}
	claimed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ? AND report_status = ? AND status IN ?", id, owner, "pending", []string{"completed", "partial", "failed", "cancelled", "interrupted"}).Updates(map[string]any{"report_status": "processing", "report_claim": claim, "report_error": "", "updated_at": tx.NowFunc()})
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		return tx.Where("id = ? AND conversation_id = ?", id, owner).Take(&row).Error
	})
	if err != nil {
		return store.SubagentTask{}, false, err
	}
	return row, claimed, nil
}
func checkReportRun(run *store.AgentRun, owner uint) error {
	if run != nil && (run.ConversationID != owner || strings.TrimSpace(run.RunID) == "" || len(run.RunID) > 64 || run.SubagentID != "" || len(run.State) == 0 || !json.Valid(run.State)) {
		return fmt.Errorf("chat: report Run does not match conversation")
	}
	return nil
}
func (s *GormReportStore) Deliver(ctx context.Context, id string, owner uint, claim string, report GeneratedReport) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := reportIdentity(id, owner, claim); err != nil {
		return err
	}
	if report.Message.Role != "assistant" || !validReportText(report.Message.Content) {
		return fmt.Errorf("chat: report must contain a bounded assistant message")
	}
	if err := checkReportRun(report.Run, owner); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		var task store.SubagentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&task).Error; err != nil {
			return err
		}
		if task.ReportStatus == "delivered" {
			return nil
		}
		if task.ReportStatus != "processing" || task.ReportClaim != claim || !reportTerminal(task.Status) {
			return ErrReportConflict
		}
		var session store.Session
		err := tx.Where("conversation_id = ? AND source_tag = ?", owner, store.SourceChat).Order("id").First(&session).Error
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
			if report.Run.RunID == task.RunID {
				return fmt.Errorf("chat: report cannot reuse child Run")
			}
			runCopy := *report.Run
			if err := tx.Create(&runCopy).Error; err != nil {
				return err
			}
			runID = runCopy.RunID
		}
		now := tx.NowFunc()
		result := tx.Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ? AND report_status = ? AND report_claim = ?", id, owner, "processing", claim).Updates(map[string]any{"report_status": "delivered", "report_claim": "", "report_run_id": runID, "report_message_id": message.ID, "report_error": clipReportError(report.Error), "reported_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrReportConflict
		}
		return appendMessageEvent(tx, owner, id, runID, message)
	})
}
func (s *GormReportStore) Fail(ctx context.Context, id string, owner uint, claim string, reason string, run *store.AgentRun) error {
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
		return fmt.Errorf("chat: report failure reason is required")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		var task store.SubagentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&task).Error; err != nil {
			return err
		}
		if task.ReportStatus == "delivered" || task.ReportStatus == "failed" {
			return nil
		}
		if task.ReportStatus != "processing" || task.ReportClaim != claim {
			return ErrReportConflict
		}
		runID := ""
		if run != nil {
			if run.RunID == task.RunID {
				return fmt.Errorf("chat: report cannot reuse child Run")
			}
			copy := *run
			if err := tx.Create(&copy).Error; err != nil {
				return err
			}
			runID = copy.RunID
		}
		now := tx.NowFunc()
		reason = clipReportError(reason)
		result := tx.Model(&store.SubagentTask{}).Where("id = ? AND conversation_id = ? AND report_status = ? AND report_claim = ?", id, owner, "processing", claim).Updates(map[string]any{"report_status": "failed", "report_claim": "", "report_run_id": runID, "report_error": reason, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrReportConflict
		}
		data, err := json.Marshal(map[string]any{"conversation_id": owner, "task_id": id, "run_id": runID, "error": reason})
		if err != nil {
			return err
		}
		return events.Append(tx, &store.ConversationEvent{ConversationID: owner, Kind: "report.failed", Key: "report:" + id + ":failed", Data: data})
	})
}
func (s *GormReportStore) InterruptProcessing(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	for {
		var rows []store.SubagentTask
		if err := s.db.WithContext(ctx).Where("report_status = ?", "processing").Order("id").Limit(100).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, task := range rows {
			if err := s.Fail(ctx, task.ID, task.ConversationID, task.ReportClaim, "automatic report was interrupted; not retried automatically", nil); err != nil {
				return err
			}
		}
	}
}
func (s *GormReportStore) AppendTurn(ctx context.Context, sessionID uint, question, answer string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if sessionID == 0 || !validReportText(question) || !validReportText(answer) {
		return fmt.Errorf("chat: session and bounded turn text are required")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session store.Session
		if err := tx.Where("id = ?", sessionID).Take(&session).Error; err != nil {
			return err
		}
		if session.ConversationID == nil || *session.ConversationID == 0 || session.SourceTag != store.SourceChat {
			return fmt.Errorf("chat: turn must belong to a chat session")
		}
		owner := *session.ConversationID
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		if err := lockReportSession(tx, &session, owner); err != nil {
			return err
		}
		for _, item := range []struct{ role, content string }{{"user", question}, {"assistant", answer}} {
			message := store.Message{SourceTag: store.SourceChat, SessionID: session.ID, Role: item.role, Content: item.content}
			if err := appendReportMessage(tx, &message); err != nil {
				return err
			}
			if err := appendMessageEvent(tx, owner, "", "", message); err != nil {
				return err
			}
		}
		return nil
	})
}
func lockReportSession(tx *gorm.DB, session *store.Session, owner uint) error {
	var locked store.Session
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ? AND source_tag = ?", session.ID, owner, store.SourceChat).Take(&locked).Error; err != nil {
		return err
	}
	*session = locked
	return nil
}
func appendReportMessage(tx *gorm.DB, message *store.Message) error {
	var last int
	if err := tx.Model(&store.Message{}).Where("session_id = ?", message.SessionID).Select("COALESCE(MAX(seq), 0)").Scan(&last).Error; err != nil {
		return err
	}
	message.Seq = last + 1
	return tx.Create(message).Error
}

// 添加消息事件 转化格式 用sse主动推
func appendMessageEvent(tx *gorm.DB, owner uint, taskID, runID string, message store.Message) error {
	data, err := json.Marshal(struct {
		ConversationID uint          `json:"conversation_id"`
		TaskID         string        `json:"task_id,omitempty"`
		RunID          string        `json:"run_id,omitempty"`
		Message        store.Message `json:"message"`
	}{owner, taskID, runID, message})
	if err != nil {
		return err
	}
	return events.Append(tx, &store.ConversationEvent{ConversationID: owner, Kind: "message.created", Key: fmt.Sprintf("message:%d", message.ID), Data: data})
}
func validReportText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 512<<10 && utf8.ValidString(value)
}
func clipReportError(value string) string {
	if len(value) <= 4096 {
		return value
	}
	return clipError(value, 4096)
}
func clipError(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}
