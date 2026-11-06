package http

import (
	"errors"
	"net/http"
	"strings"

	"recallweave/internal/run"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type RunHandler struct {
	manager *run.RunManager
}

func NewRunHandler(manager *run.RunManager) *RunHandler {
	return &RunHandler{manager: manager}
}

// 主要负责 一次  runtime 的记录的 获取 和 列表
func (h *RunHandler) GetRun(c *gin.Context) {
	runID := strings.TrimSpace(c.Param("run_id"))
	if runID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}

	run, err := h.manager.GetAgentRun(c.Request.Context(), runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load run"})
		return
	}
	c.JSON(http.StatusOK, run)
}

func (h *RunHandler) ListRuns(c *gin.Context) {
	conversationID, ok := conversationIDFromPath(c)
	if !ok {
		return
	}

	runs, err := h.manager.ListAgentRuns(c.Request.Context(), conversationID)
	if err != nil {
		if errors.Is(err, run.ErrConversationNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}
