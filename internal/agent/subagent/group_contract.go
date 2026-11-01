package subagent

import (
	"context"

	"recallweave/internal/store"
)

type GroupTaskStore interface {
	GroupsEnabled() bool
	CreateGroup(context.Context, *store.TaskGroup, []store.SubagentTask) error
	LoadGroup(context.Context, string, uint) (store.TaskGroup, []store.SubagentTask, error)
	ListGroups(context.Context, uint, int) ([]store.TaskGroup, error)
	RequestCancelGroup(context.Context, string, uint) (store.TaskGroup, []store.SubagentTask, error)
}
