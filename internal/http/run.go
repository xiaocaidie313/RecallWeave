package http

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (h *ConversationHandler) GetRun(c *gin.Context) {
	runID := strings.TrimSpace(c.Param("run_id"))
	if runID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}

	run, err := h.memory.GetAgentRun(c.Request.Context(), runID)
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

func (h *ConversationHandler) ListRuns(c *gin.Context) {
	conversationID, ok := conversationIDFromPath(c)
	if !ok {
		return
	}

	exists, err := h.memory.ConversationExists(c.Request.Context(), conversationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}

	runs, err := h.memory.ListAgentRuns(c.Request.Context(), conversationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}
