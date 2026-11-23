package http

import (
	"errors"
	"net/http"

	"recallweave/internal/thinking"
	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// MountThinkingCheckpoints exposes the user's saved starting point for the next turn.
func MountThinkingCheckpoints(router *gin.Engine, store *thinking.Store) {
	const path = "/api/conversations/:conversation_id/thinking/checkpoint"
	router.GET(path, func(c *gin.Context) {
		id, ok := conversationIDFromPath(c)
		if !ok {
			return
		}
		checkpoint, err := store.Latest(c.Request.Context(), id)
		writeThinkingCheckpoint(c, checkpoint, err)
	})
	router.POST(path, func(c *gin.Context) {
		id, ok := conversationIDFromPath(c)
		if !ok {
			return
		}
		var input struct {
			Text      string `json:"text"`
			Direction string `json:"direction"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid checkpoint JSON"})
			return
		}
		checkpoint, err := store.Save(c.Request.Context(), id, input.Text, input.Direction)
		writeThinkingCheckpoint(c, checkpoint, err)
	})
}

func writeThinkingCheckpoint(c *gin.Context, checkpoint *types.ThinkingCheckpoint, err error) {
	switch {
	case errors.Is(err, thinking.ErrInvalidCheckpoint):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to access thinking checkpoint"})
	default:
		c.JSON(http.StatusOK, gin.H{"checkpoint": checkpoint})
	}
}
