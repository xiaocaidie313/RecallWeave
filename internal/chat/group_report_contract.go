package chat

import (
	"context"

	"recallweave/internal/types"
)

type GroupReportStore interface {
	PendingGroups(context.Context, int) ([]types.TaskGroup, error)
	ClaimGroup(context.Context, string, uint, string) (types.TaskGroup, []types.SubagentTask, bool, error)
	DeliverGroup(context.Context, string, uint, string, GeneratedReport) error
	FailGroup(context.Context, string, uint, string, string, *types.AgentRun) error
	InterruptGroupProcessing(context.Context) error
}
