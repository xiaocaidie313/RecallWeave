package subagent

import (
	"context"
	"errors"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

var ErrTaskConflict = errors.New("subagent: task is no longer pending")
var ErrTaskNotFound = gorm.ErrRecordNotFound

type TaskStore interface {
	Create(context.Context, *store.SubagentTask) error
	Load(context.Context, string, uint) (store.SubagentTask, error)
	List(context.Context, uint, int) ([]store.SubagentTask, error)
	Start(context.Context, string, uint) (bool, error)
	Finish(context.Context, *store.SubagentTask, *store.AgentRun) error
	RequestCancel(context.Context, string, uint) (store.SubagentTask, error)
	InterruptUnfinished(context.Context) error
}

type TaskCheckpointVerifier interface {
	CheckTaskCheckpoint(context.Context, store.SubagentSession) error
}
