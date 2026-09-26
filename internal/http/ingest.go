package http

import (
	"errors"
	"net/http"

	"recallweave/internal/ingest"
	"recallweave/internal/store"

	"github.com/gin-gonic/gin"
)

type IngestHandler struct {
	service *ingest.Service
}

func NewIngestHandler(service *ingest.Service) *IngestHandler {
	return &IngestHandler{service: service}
}

type importTextRequest struct {
	SourceTag string `json:"source_tag" binding:"required"`
	Name      string `json:"name"`
	Text      string `json:"text" binding:"required"`
}

func (h *IngestHandler) ImportText(c *gin.Context) {
	var req importTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	result, err := h.service.ImportText(
		c.Request.Context(),
		store.SourceTag(req.SourceTag),
		req.Name,
		req.Text,
	)
	if err != nil {
		if errors.Is(err, ingest.ErrInvalidSource) || errors.Is(err, ingest.ErrEmptyText) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"session_id":    result.SessionID,
		"message_count": result.MessageCount,
	})
}
