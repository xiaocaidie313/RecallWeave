package chat

import (
	"context"

	"recallweave/internal/store"
)

type ReportStore interface {
	Pending(context.Context, int) ([]store.SubagentTask, error)
	Claim(context.Context, string, uint, string) (store.SubagentTask, bool, error)
	Deliver(context.Context, string, uint, string, GeneratedReport) error
	Fail(context.Context, string, uint, string, string, *store.AgentRun) error
	InterruptProcessing(context.Context) error
}
