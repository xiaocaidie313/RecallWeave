package subagent

import (
	"context"

	"recallweave/internal/types"
)

type GroupTaskStore interface {
	GroupsEnabled() bool
	CreateGroup(context.Context, *types.TaskGroup, []types.SubagentTask) error
	LoadGroup(context.Context, string, uint) (types.TaskGroup, []types.SubagentTask, error)
	ListGroups(context.Context, uint, int) ([]types.TaskGroup, error)
	RequestCancelGroup(context.Context, string, uint) (types.TaskGroup, []types.SubagentTask, error)
}
