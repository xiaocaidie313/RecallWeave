package http

import (
	"net/http"
	"strings"

	"recallweave/internal/memory"

	"github.com/gin-gonic/gin"
)

type ConversationHandler struct {
	memory *memory.MemoryManger
}

func NewConversationHandler(memoryManger *memory.MemoryManger) *ConversationHandler {
	return &ConversationHandler{memory: memoryManger}
}

type createConversationRequest struct {
	Title string `json:"title"`
}

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

	item, err := h.memory.CreateConversation(c.Request.Context(), title)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create conversation"})
		return
	}

	c.JSON(http.StatusCreated, item)
}

func (h *ConversationHandler) List(c *gin.Context) {
	items, err := h.memory.ListConversations(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list conversations"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"conversations": items})
}
