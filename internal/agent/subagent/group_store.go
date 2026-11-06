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

var _ GroupTaskStore = (*GormTaskStore)(nil)

// direct 成员独立交付；历史空模式的组仍保留原本等待全部成员的交付方式。
func (s *GormTaskStore) memberReportStatus(group *types.TaskGroup) (string, error) {
	if group != nil {
		mode, err := delivery.ResolveStoredGroupMode(group.DeliveryMode)
		if err != nil {
			return "", err
		}
		if mode != delivery.Direct {
			return "grouped", nil
		}
	}
	if s.events {
		return "pending", nil
	}
	return "", nil
}

func groupTaskIDs(group types.TaskGroup) ([]string, error) {
	var ids []string
	if json.Unmarshal(group.TaskIDs, &ids) != nil || len(ids) != 2 || ids[0] == ids[1] {
		return nil, fmt.Errorf("subagent: a task group must contain two distinct executions")
	}
	for _, id := range ids {
		if !subagentIDPattern.MatchString(id) {
			return nil, fmt.Errorf("subagent: invalid group member identity")
		}
	}
	return ids, nil
}

func (s *GormTaskStore) checkGroup(ctx context.Context, id string, owner uint) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if !s.GroupsEnabled() {
		return fmt.Errorf("subagent: task groups are unavailable")
	}
	if owner == 0 || !subagentIDPattern.MatchString(id) {
		return fmt.Errorf("subagent: group id and owning conversation are required")
	}
	return nil
}

func lockTaskGroup(tx *gorm.DB, id string, owner uint) (*types.TaskGroup, error) {
	var group types.TaskGroup
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&group).Error; err != nil {
		return nil, err
	}
	if group.ID != id || group.ConversationID != owner {
		return nil, fmt.Errorf("subagent: group ownership mismatch")
	}
	return &group, nil
}

func lockTaskGroupForMember(tx *gorm.DB, id string, owner uint) (*types.TaskGroup, error) {
	var member types.SubagentTask
	if err := tx.Select("id", "group_id").Where("id = ? AND conversation_id = ?", id, owner).Take(&member).Error; err != nil {
		return nil, err
	}
	if member.GroupID == "" {
		return nil, nil
	}
	return lockTaskGroup(tx, member.GroupID, owner)
}

func loadTaskGroupMembers(tx *gorm.DB, group types.TaskGroup, lock bool) ([]types.SubagentTask, error) {
	ids, err := groupTaskIDs(group)
	if err != nil {
		return nil, err
	}
	query := tx.Where("group_id = ? AND conversation_id = ? AND id IN ?", group.ID, group.ConversationID, ids).Order("id ASC")
	// 上锁
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var tasks []types.SubagentTask
	if err := query.Find(&tasks).Error; err != nil {
		return nil, err
	}
	// 组任务必须有2个成员
	if len(tasks) != 2 {
		return nil, fmt.Errorf("subagent: group membership is incomplete")
	}
	byID := make(map[string]types.SubagentTask, 2)
	for _, task := range tasks {
		if task.GroupID != group.ID || task.ConversationID != group.ConversationID || task.ParentRunID != group.ParentRunID {
			return nil, fmt.Errorf("subagent: group member ownership mismatch")
		}
		if _, duplicate := byID[task.ID]; duplicate {
			return nil, fmt.Errorf("subagent: duplicate group member")
		}
		byID[task.ID] = task
	}
	ordered := make([]types.SubagentTask, 0, 2)
	for _, id := range ids {
		task, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("subagent: group member missing")
		}
		ordered = append(ordered, task)
	}
	return ordered, nil
}

func aggregateGroupStatus(members []types.SubagentTask, cancellationRequested bool) (string, bool) {
	if len(members) != 2 {
		return "", false
	}
	allCompleted, allCancelled, allInterrupted, usable := true, true, true, false
	allCancellationRequested := true
	for _, row := range members {
		if !taskTerminalStatus(row.Status) {
			return "running", false
		}
		allCompleted = allCompleted && row.Status == "completed"
		allCancelled = allCancelled && row.Status == "cancelled"
		allInterrupted = allInterrupted && row.Status == "interrupted"
		allCancellationRequested = allCancellationRequested && row.CancelRequested
		usable = usable || row.Status == "completed" || row.Status == "partial"
	}
	switch {
	case allCompleted:
		return "completed", true
	case usable:
		return "partial", true
	case allCancelled && (cancellationRequested || allCancellationRequested):
		return "cancelled", true
	case allInterrupted:
		return "interrupted", true
	default:
		return "failed", true
	}
}

// The caller holds the conversation and group locks. Member executions have
// immutable identities, and their terminal updates take this same group lock.
func (s *GormTaskStore) refreshTaskGroup(tx *gorm.DB, group *types.TaskGroup) error {
	mode, err := delivery.ResolveStoredGroupMode(group.DeliveryMode)
	if err != nil {
		return err
	}
	members, err := loadTaskGroupMembers(tx, *group, false)
	if err != nil {
		return err
	}
	status, done := aggregateGroupStatus(members, group.CancelRequested)
	if !done {
		return nil
	}
	if taskTerminalStatus(group.Status) {
		if group.Status != status {
			return ErrTaskConflict
		}
		return nil
	}
	if group.Status != "running" {
		return fmt.Errorf("subagent: invalid group state")
	}
	now := tx.NowFunc()
	updated := *group
	updated.Status = status
	updated.FinishedAt = &now
	updated.UpdatedAt = now
	fields := []string{"status", "finished_at", "updated_at"}
	if mode == delivery.Direct {
		updated.ReportStatus = "not_required"
		fields = append(fields, "report_status")
	} else if s.events {
		updated.ReportStatus = "pending"
		fields = append(fields, "report_status")
	}
	result := tx.Model(&types.TaskGroup{}).Where("id = ? AND conversation_id = ? AND status = ?", group.ID, group.ConversationID, "running").Select(fields).Updates(&updated)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskConflict
	}
	if s.events {
		data, err := json.Marshal(struct {
			ConversationID uint            `json:"conversation_id"`
			GroupID        string          `json:"group_id"`
			Status         string          `json:"status"`
			DeliveryMode   string          `json:"delivery_mode"`
			TaskIDs        json.RawMessage `json:"task_ids"`
		}{
			ConversationID: updated.ConversationID,
			GroupID:        updated.ID,
			Status:         updated.Status,
			DeliveryMode:   mode,
			TaskIDs:        updated.TaskIDs,
		})
		if err != nil {
			return err
		}
		if err := events.Append(tx, &types.ConversationEvent{
			ConversationID: updated.ConversationID,
			Kind:           "task_group.finished",
			Key:            "group:" + updated.ID + ":finished",
			Data:           data,
		}); err != nil {
			return err
		}
	}
	*group = updated
	return nil
}

func (s *GormTaskStore) CreateGroup(ctx context.Context, group *types.TaskGroup, members []types.SubagentTask) error {
	if group == nil {
		return fmt.Errorf("subagent: group is required")
	}
	if err := s.checkGroup(ctx, group.ID, group.ConversationID); err != nil {
		return err
	}
	ids, err := groupTaskIDs(*group)
	if err != nil {
		return err
	}
	mode, err := delivery.NormalizeNewGroupMode(group.DeliveryMode)
	if err != nil {
		return err
	}
	if !validTaskID(group.ParentRunID, false) || strings.TrimSpace(group.Goal) == "" || len(group.Goal) > 8<<10 || !utf8.ValidString(group.Goal) || len(group.ExpectedOutput) > 2<<10 || !utf8.ValidString(group.ExpectedOutput) {
		return fmt.Errorf("subagent: bounded group goal and parent are required")
	}
	validReportStatus := group.ReportStatus == "" || mode == delivery.Direct && group.ReportStatus == "not_required"
	if group.Status != "running" || group.CancelRequested || group.FinishedAt != nil || !validReportStatus || group.ReportClaim != "" || group.ReportRunID != "" || group.ReportMessageID != 0 || group.ReportError != "" || group.ReportedAt != nil {
		return fmt.Errorf("subagent: a new group must be running and unreported")
	}
	if len(members) != 2 {
		return fmt.Errorf("subagent: exactly two members are required")
	}
	copyMembers := append([]types.SubagentTask(nil), members...)
	for index, member := range copyMembers {
		if err := checkTaskRecord(&member); err != nil {
			return err
		}
		memberMode, _ := delivery.NormalizeTaskMode(member.DeliveryMode)
		validMemberReport := member.ReportStatus == "" || mode != delivery.Direct && member.ReportStatus == "grouped"
		if member.ID != ids[index] || !subagentIDPattern.MatchString(member.SubagentID) || member.GroupID != group.ID || member.ConversationID != group.ConversationID || member.ParentRunID != group.ParentRunID || member.Status != "queued" || member.RunID != "" || member.PreviousRunID != "" || member.CancelRequested || len(member.Result) > 0 || member.StartedAt != nil || member.FinishedAt != nil || memberMode != delivery.Direct || !validMemberReport || member.ReportClaim != "" || member.ReportRunID != "" || member.ReportMessageID != 0 || member.ReportError != "" || member.ReportedAt != nil {
			return fmt.Errorf("subagent: invalid new group member")
		}
		copyMembers[index].DeliveryMode = delivery.Direct
		copyMembers[index].ReportStatus = ""
		if mode != delivery.Direct {
			copyMembers[index].ReportStatus = "grouped"
		}
	}
	if copyMembers[0].SubagentID == copyMembers[1].SubagentID {
		return fmt.Errorf("subagent: group children need independent contexts")
	}
	record := *group
	record.DeliveryMode = mode
	if mode == delivery.Direct {
		record.ReportStatus = "not_required"
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, group.ConversationID); err != nil {
			return err
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return tx.Create(&copyMembers).Error
	})
	if err == nil {
		*group = record
		copy(members, copyMembers)
	}
	return err
}

func (s *GormTaskStore) LoadGroup(ctx context.Context, id string, owner uint) (types.TaskGroup, []types.SubagentTask, error) {
	var group types.TaskGroup
	var members []types.SubagentTask
	if err := s.checkGroup(ctx, id, owner); err != nil {
		return group, nil, err
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND conversation_id = ?", id, owner).Take(&group).Error; err != nil {
			return err
		}
		if group.ID != id || group.ConversationID != owner {
			return fmt.Errorf("subagent: group ownership mismatch")
		}
		var err error
		members, err = loadTaskGroupMembers(tx, group, false)
		return err
	})
	return group, members, err
}

func (s *GormTaskStore) ListGroups(ctx context.Context, owner uint, limit int) ([]types.TaskGroup, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if !s.GroupsEnabled() || owner == 0 {
		return nil, fmt.Errorf("subagent: task groups and owning conversation are required")
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	rows := make([]types.TaskGroup, 0)
	err := s.db.WithContext(ctx).Where("conversation_id = ?", owner).Order("created_at DESC").Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s *GormTaskStore) RequestCancelGroup(ctx context.Context, id string, owner uint) (types.TaskGroup, []types.SubagentTask, error) {
	var group types.TaskGroup
	var members []types.SubagentTask
	if err := s.checkGroup(ctx, id, owner); err != nil {
		return group, nil, err
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := events.LockConversation(tx, owner); err != nil {
			return err
		}
		locked, err := lockTaskGroup(tx, id, owner)
		if err != nil {
			return err
		}
		group = *locked
		members, err = loadTaskGroupMembers(tx, group, true)
		if err != nil {
			return err
		}
		if taskTerminalStatus(group.Status) {
			return nil
		}
		if group.Status != "running" {
			return fmt.Errorf("subagent: invalid typesd group state")
		}
		now := tx.NowFunc()
		group.CancelRequested = true
		group.UpdatedAt = now
		result := tx.Model(&types.TaskGroup{}).Where("id = ? AND conversation_id = ? AND status = ?", id, owner, "running").Updates(map[string]any{"cancel_requested": true, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrTaskConflict
		}
		for index, member := range members {
			if taskTerminalStatus(member.Status) {
				continue
			}
			if member.Status != "queued" && member.Status != "running" {
				return fmt.Errorf("subagent: invalid group member state")
			}
			oldStatus := member.Status
			member.CancelRequested = true
			member.UpdatedAt = now
			fields := []string{"cancel_requested", "updated_at"}
			if oldStatus == "queued" {
				member.Status = "cancelled"
				member.FinishedAt = &now
				member.ReportStatus, err = s.memberReportStatus(&group)
				if err != nil {
					return err
				}
				fields = append(fields, "status", "finished_at", "report_status")
			}
			result := tx.Model(&types.SubagentTask{}).Where("id = ? AND conversation_id = ? AND group_id = ? AND status = ?", member.ID, owner, id, oldStatus).Select(fields).Updates(&member)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrTaskConflict
			}
			if s.events && oldStatus == "queued" {
				if err := appendTaskFinished(tx, member); err != nil {
					return err
				}
			}
			members[index] = member
		}
		return s.refreshTaskGroup(tx, &group)
	})
	return group, members, err
}
