package chat

import (
	"context"

	"recallweave/internal/delivery"
	"recallweave/internal/types"

	"gorm.io/gorm"
)

var _ deliveryQueue = (*GormReportStore)(nil)
var _ groupDeliveryQueue = (*GormReportStore)(nil)

// Select each delivery lane independently so a backlog of model summaries
// cannot hide ready results behind the polling batch limit.
func (s *GormReportStore) deliveryQueue(ctx context.Context, limit int, synthesize bool, excludedConversations []uint, grouped bool) *gorm.DB {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := s.db.WithContext(ctx).Select("id", "conversation_id", "delivery_mode").
		Where("report_status = ? AND status IN ?", "pending", []string{"completed", "partial", "failed", "cancelled", "interrupted"})
	if synthesize {
		query = query.Where("delivery_mode = ?", delivery.Synthesize)
	} else {
		// Empty/NULL tasks are direct; empty/NULL legacy groups are combined.
		// Unknown modes reach the generator and fail explicitly instead of
		// remaining permanently invisible in a queue.
		query = query.Where("COALESCE(delivery_mode, '') <> ?", delivery.Synthesize)
	}
	if grouped {
		query = query.Where("COALESCE(delivery_mode, '') <> ?", delivery.Direct)
	}
	if len(excludedConversations) != 0 {
		query = query.Where("conversation_id NOT IN ?", excludedConversations)
	}
	return query.Order("finished_at ASC").Order("id ASC").Limit(limit)
}

func (s *GormReportStore) PendingDelivery(ctx context.Context, limit int, synthesize bool, excludedConversations []uint) ([]types.SubagentTask, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	rows := make([]types.SubagentTask, 0)
	err := s.deliveryQueue(ctx, limit, synthesize, excludedConversations, false).Find(&rows).Error
	return rows, err
}

func (s *GormReportStore) PendingGroupDelivery(ctx context.Context, limit int, synthesize bool, excludedConversations []uint) ([]types.TaskGroup, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	rows := make([]types.TaskGroup, 0)
	err := s.deliveryQueue(ctx, limit, synthesize, excludedConversations, true).Find(&rows).Error
	return rows, err
}
