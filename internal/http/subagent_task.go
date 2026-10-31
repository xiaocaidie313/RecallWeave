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

// 子agent 任务服务
type SubagentTaskService interface {
	GetTask(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error)
	ListTasks(context.Context, uint) ([]runtime.BackgroundTask, error)
	CancelTask(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error)
}

type SubagentTaskHandler struct {
	service SubagentTaskService
}

func NewSubagentTaskHandler(service SubagentTaskService) *SubagentTaskHandler {
	return &SubagentTaskHandler{service: service}
}

func MountSubagentTasks(router *gin.Engine, handler *SubagentTaskHandler) {
	group := router.Group("/api/conversations/:conversation_id/subagent-tasks")
	group.GET("", handler.List)
	group.GET("/:task_id", handler.Get)
	group.POST("/:task_id/cancel", handler.Cancel)
}

var subagentTaskIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (h *SubagentTaskHandler) List(c *gin.Context) {
	owner, ok := conversationIDFromPath(c)
	if !ok || !h.available(c) {
		return
	}
	tasks, err := h.service.ListTasks(c.Request.Context(), owner)
	if err != nil {
		writeSubagentTaskError(c, err, "failed to list background tasks")
		return
	}
	for _, task := range tasks {
		if task.ConversationID != owner {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list background tasks"})
			return
		}
	}
	if tasks == nil {
		tasks = make([]runtime.BackgroundTask, 0)
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

func (h *SubagentTaskHandler) Get(c *gin.Context)    { h.single(c, false) }
func (h *SubagentTaskHandler) Cancel(c *gin.Context) { h.single(c, true) }

// 单个任务 获取或取消
func (h *SubagentTaskHandler) single(c *gin.Context, cancel bool) {
	owner, ok := conversationIDFromPath(c)
	if !ok {
		return
	}
	id, ok := subagentTaskIDFromPath(c)
	if !ok || !h.available(c) {
		return
	}
	// 标记父亲
	parent := runtime.DelegationParent{ConversationID: owner}
	var task runtime.BackgroundTask
	var err error
	message := "failed to load background task"
	if cancel {
		message = "failed to cancel background task"
		task, err = h.service.CancelTask(c.Request.Context(), parent, id)
	} else {
		task, err = h.service.GetTask(c.Request.Context(), parent, id)
	}
	if err != nil {
		writeSubagentTaskError(c, err, message)
		return
	}
	if task.ConversationID != owner || task.TaskID != id {
		c.JSON(http.StatusInternalServerError, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *SubagentTaskHandler) available(c *gin.Context) bool {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "background tasks are unavailable"}) //503
		return false
	}
	return true
}

func subagentTaskIDFromPath(c *gin.Context) (string, bool) {
	// path/subagent/xx/{task_id}
	id := c.Param("task_id")
	if !subagentTaskIDPattern.MatchString(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"}) //400
		return "", false
	}
	return id, true
}

func writeSubagentTaskError(c *gin.Context, err error, message string) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "background task not found"}) //404
	case errors.Is(err, subagent.ErrBackgroundUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "background tasks are unavailable"}) //503
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": message}) //500
	}
}
