package http

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"recallweave/internal/chat"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ChatHandler struct {
	agent *chat.ChatService
}

func NewChatHandler(agent *chat.ChatService) *ChatHandler {
	return &ChatHandler{agent: agent}
}

type chatRequest struct {
	Question   string              `json:"question"`
	Mode       string              `json:"mode"`
	Source     *chat.ThoughtSource `json:"source"`
	ResumeFrom *uint               `json:"resume_from"`
}

// Chat dispatches ordinary questions and explicit thinking-partner turns.
func (h *ChatHandler) Chat(c *gin.Context) {
	conversationID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid conversation id",
		})
		return
	}

	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	question := strings.TrimSpace(req.Question)
	var answer chat.Answer
	switch req.Mode {
	case "", "chat":
		if req.Source != nil || req.ResumeFrom != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source and resume_from require thinking mode"})
			return
		}
		if question == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "question is empty"})
			return
		}
		answer, err = h.agent.Chat(c.Request.Context(), question, uint(conversationID))
	case "thinking":
		if conversationID == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
			return
		}
		options := chat.ThinkingOptions{Source: req.Source}
		if req.ResumeFrom != nil {
			if *req.ResumeFrom == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "resume_from must be a positive conversation id"})
				return
			}
			options.ResumeFrom = *req.ResumeFrom
		}
		answer, err = h.agent.Think(c.Request.Context(), question, uint(conversationID), options)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be chat or thinking"})
		return
	}
	switch {
	case errors.Is(err, chat.ErrInvalidThoughtSource), errors.Is(err, chat.ErrEmptyThought):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, chat.ErrCheckpointNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "thinking checkpoint not found"})
	case errors.Is(err, chat.ErrThoughtSourceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "source messages not found"})
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
	case errors.Is(err, chat.ErrNoModel), errors.Is(err, chat.ErrThinkingUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusOK, answer)
	}
}
