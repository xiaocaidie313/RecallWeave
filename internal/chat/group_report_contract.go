package chat

import (
	"context"

	"recallweave/internal/store"
)

type GroupReportStore interface {
	PendingGroups(context.Context, int) ([]store.TaskGroup, error)
	ClaimGroup(context.Context, string, uint, string) (store.TaskGroup, []store.SubagentTask, bool, error)
	DeliverGroup(context.Context, string, uint, string, GeneratedReport) error
	FailGroup(context.Context, string, uint, string, string, *store.AgentRun) error
	InterruptGroupProcessing(context.Context) error
}
