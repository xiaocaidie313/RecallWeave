package chat

import (
	"context"

	"recallweave/internal/types"
)

type ReportStore interface {
	Pending(context.Context, int) ([]types.SubagentTask, error)
	Claim(context.Context, string, uint, string) (types.SubagentTask, bool, error)
	Deliver(context.Context, string, uint, string, GeneratedReport) error
	Fail(context.Context, string, uint, string, string, *types.AgentRun) error
	InterruptProcessing(context.Context) error
}
