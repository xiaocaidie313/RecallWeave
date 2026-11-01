package http

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/agent/subagent"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TaskGroupService interface {
	GetGroup(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error)
	ListGroups(context.Context, uint) ([]runtime.TaskGroup, error)
	CancelGroup(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error)
}

type TaskGroupHandler struct{ service TaskGroupService }

func NewTaskGroupHandler(service TaskGroupService) *TaskGroupHandler {
	return &TaskGroupHandler{service: service}
}

func MountTaskGroups(router *gin.Engine, handler *TaskGroupHandler) {
	group := router.Group("/api/conversations/:conversation_id/task-groups")
	group.GET("", handler.List)
	group.GET("/:group_id", handler.Get)
	group.POST("/:group_id/cancel", handler.Cancel)
}

var taskGroupIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (h *TaskGroupHandler) available(c *gin.Context) bool {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task groups are unavailable"})
		return false
	}
	return true
}

func (h *TaskGroupHandler) List(c *gin.Context) {
	owner, ok := conversationIDFromPath(c)
	if !ok || !h.available(c) {
		return
	}
	groups, err := h.service.ListGroups(c.Request.Context(), owner)
	if err != nil {
		writeTaskGroupError(c, err, "failed to list task groups")
		return
	}
	for _, group := range groups {
		if group.ConversationID != owner {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list task groups"})
			return
		}
	}
	if groups == nil {
		groups = make([]runtime.TaskGroup, 0)
	}
	c.JSON(http.StatusOK, gin.H{"groups": groups})
}

func (h *TaskGroupHandler) Get(c *gin.Context)    { h.single(c, false) }
func (h *TaskGroupHandler) Cancel(c *gin.Context) { h.single(c, true) }

func (h *TaskGroupHandler) single(c *gin.Context, cancel bool) {
	owner, ok := conversationIDFromPath(c)
	if !ok {
		return
	}
	id := c.Param("group_id")
	if !taskGroupIDPattern.MatchString(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group id"})
		return
	}
	if !h.available(c) {
		return
	}
	parent := runtime.DelegationParent{ConversationID: owner}
	var group runtime.TaskGroup
	var err error
	message := "failed to load task group"
	if cancel {
		message = "failed to cancel task group"
		group, err = h.service.CancelGroup(c.Request.Context(), parent, id)
	} else {
		group, err = h.service.GetGroup(c.Request.Context(), parent, id)
	}
	if err != nil {
		writeTaskGroupError(c, err, message)
		return
	}
	if group.ConversationID != owner || group.GroupID != id {
		c.JSON(http.StatusInternalServerError, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusOK, group)
}

func writeTaskGroupError(c *gin.Context, err error, message string) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "task group not found"})
	case errors.Is(err, subagent.ErrBackgroundUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task groups are unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": message})
	}
}
