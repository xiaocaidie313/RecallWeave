package http

import (
	"errors"
	"net/http"

	"recallweave/internal/memory"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (h *ConversationHandler) Fork(c *gin.Context) {
	parentID, ok := conversationIDFromPath(c)
	if !ok {
		return
	}
	var request struct {
		FromSeq int    `json:"from_seq" binding:"required,min=1"`
		Title   string `json:"title"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from_seq must be a positive message number"})
		return
	}
	result, err := h.memory.ForkConversation(c.Request.Context(), parentID, request.FromSeq, request.Title)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "parent conversation or chat session not found"})
	case errors.Is(err, memory.ErrInvalidFork):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, memory.ErrForkTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fork conversation"})
	default:
		c.JSON(http.StatusCreated, result)
	}
}
