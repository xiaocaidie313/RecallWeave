package chat

import (
	"context"

	"recallweave/internal/delivery"
	"recallweave/internal/store"
)

// Mode-filtered queues keep pending synthesis work from hiding ready deliveries
// behind a limited page. Existing ReportStore adapters remain supported.
type deliveryQueue interface {
	PendingDelivery(context.Context, int, bool, []uint) ([]store.SubagentTask, error)
}

type groupDeliveryQueue interface {
	PendingGroupDelivery(context.Context, int, bool, []uint) ([]store.TaskGroup, error)
}

func requiresSynthesis(mode string) bool { return mode == delivery.Synthesize }

func (d *ReportDispatcher) slots(synthesize bool) chan struct{} {
	if synthesize {
		return d.synthesisSlots
	}
	return d.deliverySlots
}

func (d *ReportDispatcher) hasCapacity(synthesize bool) bool {
	slots := d.slots(synthesize)
	return d.ctx.Err() == nil && len(slots) < cap(slots)
}

func (d *ReportDispatcher) busyConversations() []uint {
	d.mu.Lock()
	defer d.mu.Unlock()
	owners := make([]uint, 0, len(d.inFlight))
	for owner := range d.inFlight {
		owners = append(owners, owner)
	}
	return owners
}

func (d *ReportDispatcher) startDelivery(owner uint, synthesize bool, work func(func())) bool {
	if d.ctx.Err() != nil {
		return false
	}
	slots := d.slots(synthesize)
	select {
	case slots <- struct{}{}:
	default:
		return false
	}
	release, ok := d.agent.TryReportLock(owner)
	if !ok {
		<-slots
		return false
	}
	if d.ctx.Err() != nil {
		release()
		<-slots
		return false
	}
	d.mu.Lock()
	// The previous worker may have released the gate before retiring its slot.
	// Let it remove its entry before a new worker can own this conversation.
	if d.inFlight[owner] {
		d.mu.Unlock()
		release()
		<-slots
		return false
	}
	d.inFlight[owner] = true
	d.mu.Unlock()
	// Only the scanner calls Add. Its loop waits after dispatching has stopped.
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer func() {
			d.mu.Lock()
			delete(d.inFlight, owner)
			d.mu.Unlock()
			<-slots
			select {
			case d.wake <- struct{}{}:
			default:
			}
		}()
		work(release)
	}()
	return true
}

func excludedOwner(owners []uint, owner uint) bool {
	for _, existing := range owners {
		if existing == owner {
			return true
		}
	}
	return false
}

func (d *ReportDispatcher) pendingReports(ctx context.Context, synthesize bool, excluded []uint) ([]store.SubagentTask, error) {
	if queue, ok := d.store.(deliveryQueue); ok {
		return queue.PendingDelivery(ctx, 20, synthesize, excluded)
	}
	rows, err := d.store.Pending(ctx, 100)
	if err != nil {
		return nil, err
	}
	filtered := make([]store.SubagentTask, 0, len(rows))
	for _, row := range rows {
		if requiresSynthesis(row.DeliveryMode) == synthesize && !excludedOwner(excluded, row.ConversationID) {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}

func (d *ReportDispatcher) pendingGroups(ctx context.Context, synthesize bool, excluded []uint) ([]store.TaskGroup, error) {
	if queue, ok := d.groups.(groupDeliveryQueue); ok {
		return queue.PendingGroupDelivery(ctx, 20, synthesize, excluded)
	}
	rows, err := d.groups.PendingGroups(ctx, 100)
	if err != nil {
		return nil, err
	}
	filtered := make([]store.TaskGroup, 0, len(rows))
	for _, row := range rows {
		if row.DeliveryMode != delivery.Direct && requiresSynthesis(row.DeliveryMode) == synthesize && !excludedOwner(excluded, row.ConversationID) {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}
