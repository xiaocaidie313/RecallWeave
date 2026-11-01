package chat

import (
	"context"
	"log/slog"
	"time"

	"recallweave/internal/store"
)

func (d *ReportDispatcher) scanGroups(synthesize bool) {
	if d.groups == nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	defer cancel()
	excluded := d.busyConversations()
	for ctx.Err() == nil && d.hasCapacity(synthesize) {
		rows, err := d.pendingGroups(ctx, synthesize, excluded)
		if err != nil {
			if d.ctx.Err() == nil {
				slog.Warn("load pending group reports failed", "error", err)
			}
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, row := range rows {
			excluded = append(excluded, row.ConversationID)
			if !d.hasCapacity(synthesize) || ctx.Err() != nil {
				return
			}
			row := row
			d.startDelivery(row.ConversationID, synthesize, func(release func()) { d.processGroup(row, release) })
		}
	}
}

func (d *ReportDispatcher) processGroup(row store.TaskGroup, release func()) {
	defer release()
	claim, err := reportClaimID()
	if err != nil {
		slog.Error("create group report claim failed", "error", err)
		return
	}
	claimed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("group report panicked", "group_id", row.ID)
			if claimed {
				d.failGroup(row, claim, "综合回报异常中断，未自动重试。", nil)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	group, members, ok, err := d.groups.ClaimGroup(ctx, row.ID, row.ConversationID, claim)
	cancel()
	if err != nil {
		if d.ctx.Err() == nil {
			slog.Warn("claim group report failed", "group_id", row.ID, "error", err)
		}
		if d.ctx.Err() == nil || requiresSynthesis(row.DeliveryMode) {
			d.failGroup(row, claim, "领取综合回报时无法确认存储状态，未自动重新生成。", nil)
		}
		return
	}
	if !ok {
		return
	}
	claimed = true
	if group.ID != row.ID || group.ConversationID != row.ConversationID {
		d.failGroup(row, claim, "综合回报所属对话不匹配。", nil)
		return
	}
	ctx, cancel = context.WithTimeout(d.ctx, d.config.Timeout)
	generated, err := d.agent.GenerateGroupReport(ctx, group, members)
	cancel()
	if d.ctx.Err() != nil {
		if requiresSynthesis(group.DeliveryMode) {
			d.failGroup(row, claim, "服务停止，综合回报中断；未自动重试模型。", generated.Run)
		}
		return
	}
	if err != nil {
		d.failGroup(row, claim, "综合回报失败："+err.Error(), generated.Run)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	err = d.groups.DeliverGroup(ctx, row.ID, row.ConversationID, claim, generated)
	cancel()
	if err != nil {
		slog.Error("persist group report failed", "group_id", row.ID, "error", err)
		d.failGroup(row, claim, "综合回报保存失败："+err.Error(), generated.Run)
	}
}

func (d *ReportDispatcher) failGroup(row store.TaskGroup, claim, message string, run *store.AgentRun) {
	message = clipReportText(message, 2048)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.groups.FailGroup(ctx, row.ID, row.ConversationID, claim, message, run); err != nil {
		slog.Error("save group report failure failed", "group_id", row.ID, "error", err)
	}
}
