package http

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"recallweave/internal/memory"
	"recallweave/internal/session"
	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type SessionHandler struct {
	sessions *session.SessionManager
	memories *memory.MemoryManger
}

func NewSessionHandler(sessions *session.SessionManager, memories *memory.MemoryManger) *SessionHandler {
	return &SessionHandler{sessions: sessions, memories: memories}
}

func (h *SessionHandler) GetSession(c *gin.Context) {
	sessionID, ok := sessionIDFromPath(c)
	if !ok {
		return
	}

	overview, err := h.sessions.SessionOverview(c.Request.Context(), sessionID)
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

func (h *SessionHandler) ListSessionMemories(c *gin.Context) {
	sessionID, ok := sessionIDFromPath(c)
	if !ok {
		return
	}

	if _, err := h.sessions.GetSession(c.Request.Context(), sessionID); err != nil {
		writeSessionError(c, err)
		return
	}

	memories, err := h.memories.ListBySession(c.Request.Context(), sessionID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list memories"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"memories": memoryViews(memories)})
}

func (h *SessionHandler) ListSessionMessages(c *gin.Context) {
	sessionID, ok := sessionIDFromPath(c)
	if !ok {
		return
	}

	session, err := h.sessions.GetSession(c.Request.Context(), sessionID)
	if err != nil {
		writeSessionError(c, err)
		return
	}

	messages, err := h.sessions.ListMessages(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list messages"})
		return
	}
	deliveries, err := h.sessions.MessageDeliveries(c.Request.Context(), session, messages)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load message provenance"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"messages": messageViews(messages, deliveries)})
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
	ID                 uint               `json:"id"`
	SessionID          uint               `json:"session_id"`
	SeqStart           int                `json:"seq_start"`
	SeqEnd             int                `json:"seq_end"`
	Title              string             `json:"title"`
	Content            string             `json:"content"`
	Tag                types.MemoryTag    `json:"tag"`
	ContentType        types.ContentType  `json:"content_type"`
	Status             types.MemoryStatus `json:"status"`
	DeactivatedAt      *time.Time         `json:"deactivated_at,omitempty"`
	DeactivationReason string             `json:"deactivation_reason,omitempty"`
}

func memoryViews(memories []types.Memory) []memoryView {
	views := make([]memoryView, 0, len(memories))
	for _, item := range memories {
		views = append(views, memoryView{
			ID:                 item.ID,
			SessionID:          item.SessionID,
			SeqStart:           item.SeqStart,
			SeqEnd:             item.SeqEnd,
			Title:              item.Title,
			Content:            item.Content,
			Tag:                item.Tag,
			ContentType:        types.NormalizeContentType(string(item.ContentType)),
			Status:             item.Status,
			DeactivatedAt:      item.DeactivatedAt,
			DeactivationReason: item.DeactivationReason,
		})
	}
	return views
}

type messageView struct {
	ID        uint   `json:"id"`
	SessionID uint   `json:"session_id"`
	Seq       int    `json:"seq"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	session.MessageDelivery
}

func messageViews(messages []types.Message, deliveries map[uint]session.MessageDelivery) []messageView {
	views := make([]messageView, 0, len(messages))
	for _, item := range messages {
		views = append(views, messageView{
			ID:              item.ID,
			SessionID:       item.SessionID,
			Seq:             item.Seq,
			Role:            item.Role,
			Content:         item.Content,
			MessageDelivery: deliveries[item.ID],
		})
	}
	return views
}
