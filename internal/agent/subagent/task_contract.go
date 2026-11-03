package subagent

import (
	"context"
	"errors"

	"recallweave/internal/types"

	"gorm.io/gorm"
)

var ErrTaskConflict = errors.New("subagent: task is no longer pending")
var ErrTaskNotFound = gorm.ErrRecordNotFound

type TaskStore interface {
	Create(context.Context, *types.SubagentTask) error
	Load(context.Context, string, uint) (types.SubagentTask, error)
	List(context.Context, uint, int) ([]types.SubagentTask, error)
	Start(context.Context, string, uint) (bool, error)
	Finish(context.Context, *types.SubagentTask, *types.AgentRun) error
	RequestCancel(context.Context, string, uint) (types.SubagentTask, error)
	InterruptUnfinished(context.Context) error
}

type TaskCheckpointVerifier interface {
	CheckTaskCheckpoint(context.Context, types.SubagentSession) error
}
