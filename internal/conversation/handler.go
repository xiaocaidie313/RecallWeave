package conversation

import (
	"net/http"
	"strings"

	"recallweave/internal/store"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ConversationHandler struct {
	db *gorm.DB
}

func NewConversationHandler(db *gorm.DB) *ConversationHandler {
	return &ConversationHandler{db: db}
}

type createRequest struct {
	Title string `json:"title"`
}

// Create creates a conversation and returns its persistent ID for subsequent ask requests.
func (h *ConversationHandler) Create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "新对话"
	}

	item := store.Conversation{Title: title}
	if err := h.db.WithContext(c.Request.Context()).Create(&item).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create conversation"})
		return
	}

	c.JSON(http.StatusCreated, item)
}

// List returns conversations ordered by most recently updated.
func (h *ConversationHandler) List(c *gin.Context) {
	var items []store.Conversation
	if err := h.db.WithContext(c.Request.Context()).
		Order("updated_at DESC, id DESC").
		Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list conversations"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"conversations": items})
}
