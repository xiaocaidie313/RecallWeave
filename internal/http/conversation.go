package http

import (
	"net/http"
	"strings"

	"recallweave/internal/conversation"
	"recallweave/internal/session"

	"github.com/gin-gonic/gin"
)

const (
	defaultListLimit = 20
)

// ConversationHandler handles conversation metadata, branches, and chat history.
type ConversationHandler struct {
	manager  *conversation.ConversationManager
	sessions *session.SessionManager
}

func NewConversationHandler(manager *conversation.ConversationManager, sessions *session.SessionManager) *ConversationHandler {
	return &ConversationHandler{manager: manager, sessions: sessions}
}

type createConversationRequest struct {
	Title string `json:"title"`
}

// 创建对话
func (h *ConversationHandler) Create(c *gin.Context) {
	var req createConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "新对话"
	}

	item, err := h.manager.CreateConversation(c.Request.Context(), title)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create conversation"})
		return
	}

	c.JSON(http.StatusCreated, item)
}

// 列出对话
func (h *ConversationHandler) List(c *gin.Context) {
	items, err := h.manager.ListConversations(c.Request.Context(), defaultListLimit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list conversations"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"conversations": items})
}

// 列出对话并显示标题
func (h *ConversationHandler) ListWithTitle(c *gin.Context) {
	items, err := h.manager.ListConversationsWithTitle(c.Request.Context(), defaultListLimit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list conversations with title"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"conversations": items,
	})
}

func (h *ConversationHandler) ListNotes(c *gin.Context) {
	id, ok := conversationIDFromPath(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	exists, err := h.manager.ConversationExists(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load conversation"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}
	notes, err := h.sessions.ListLocalNotes(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list notes"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"notes": notes})
}
