package http

import (
	"errors"
	"net/http"
	"strconv"

	"recallweave/internal/store"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (h *ConversationHandler) GetSession(c *gin.Context) {
	sessionID, ok := sessionIDFromPath(c)
	if !ok {
		return
	}

	overview, err := h.memory.SessionOverview(c.Request.Context(), sessionID)
	if err != nil {
		writeSessionError(c, err)
		return
	}

	session := overview.Session
	c.JSON(http.StatusOK, gin.H{
		"id":              session.ID,
		"conversation_id": session.ConversationID,
		"source_tag":      session.SourceTag,
		"name":            session.Name,
		"extract_status":  session.ExtractStatus,
		"extract_error":   session.ExtractError,
		"extracted_at":    session.ExtractedAt,
		"message_count":   overview.MessageCount,
		"memory_count":    overview.MemoryCount,
		"created_at":      session.CreatedAt,
		"updated_at":      session.UpdatedAt,
	})
}

func (h *ConversationHandler) ListSessionMemories(c *gin.Context) {
	sessionID, ok := sessionIDFromPath(c)
	if !ok {
		return
	}

	if _, err := h.memory.GetSession(c.Request.Context(), sessionID); err != nil {
		writeSessionError(c, err)
		return
	}

	memories, err := h.memory.ListBySession(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list memories"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"memories": memoryViews(memories)})
}

func (h *ConversationHandler) ListSessionMessages(c *gin.Context) {
	sessionID, ok := sessionIDFromPath(c)
	if !ok {
		return
	}

	if _, err := h.memory.GetSession(c.Request.Context(), sessionID); err != nil {
		writeSessionError(c, err)
		return
	}

	messages, err := h.memory.ListMessages(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list messages"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"messages": messageViews(messages)})
}

func (h *ConversationHandler) ListConversationMessages(c *gin.Context) {
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

	session, err := h.memory.FindChatSession(c.Request.Context(), conversationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load chat session"})
		return
	}
	if session == nil {
		c.JSON(http.StatusOK, gin.H{"messages": []any{}})
		return
	}

	messages, err := h.memory.ListMessages(c.Request.Context(), session.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list messages"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id": session.ID,
		"messages":   messageViews(messages),
	})
}

func sessionIDFromPath(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("session_id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return 0, false
	}
	return uint(id), true
}

func writeSessionError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

type memoryView struct {
	ID        uint            `json:"id"`
	SessionID uint            `json:"session_id"`
	SeqStart  int             `json:"seq_start"`
	SeqEnd    int             `json:"seq_end"`
	Title     string          `json:"title"`
	Content   string          `json:"content"`
	Tag       store.MemoryTag `json:"tag"`
}

func memoryViews(memories []store.Memory) []memoryView {
	views := make([]memoryView, 0, len(memories))
	for _, item := range memories {
		views = append(views, memoryView{
			ID:        item.ID,
			SessionID: item.SessionID,
			SeqStart:  item.SeqStart,
			SeqEnd:    item.SeqEnd,
			Title:     item.Title,
			Content:   item.Content,
			Tag:       item.Tag,
		})
	}
	return views
}

type messageView struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

func messageViews(messages []store.Message) []messageView {
	views := make([]messageView, 0, len(messages))
	for _, item := range messages {
		views = append(views, messageView{
			Seq:     item.Seq,
			Role:    item.Role,
			Content: item.Content,
		})
	}
	return views
}
