package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (h *ConversationHandler) ListConversationMessages(c *gin.Context) {
	conversationID, ok := conversationIDFromPath(c)
	if !ok {
		return
	}

	conversation, err := h.manager.GetConversation(c.Request.Context(), conversationID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation"})
		return
	}
	inherited, err := conversation.InheritedMessages()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid fork snapshot"})
		return
	}

	session, err := h.sessions.FindChatSession(c.Request.Context(), conversationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load chat session"})
		return
	}
	if session == nil {
		c.JSON(http.StatusOK, gin.H{"messages": []any{}, "inherited_messages": inherited})
		return
	}

	messages, err := h.sessions.ListMessages(c.Request.Context(), session.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list messages"})
		return
	}
	deliveries, err := h.sessions.MessageDeliveries(c.Request.Context(), *session, messages)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load message provenance"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id":         session.ID,
		"messages":           messageViews(messages, deliveries),
		"inherited_messages": inherited,
	})
}
