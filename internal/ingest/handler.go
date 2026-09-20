package ingest

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type IngestHandler struct {
	service *IngestService
}

func NewIngestHandler(service *IngestService) *IngestHandler {
	return &IngestHandler{service: service}
}

type importTextRequest struct {
	Source string `json:"source" binding:"required"`
	Text   string `json:"text" binding:"required"`
}

func (h *IngestHandler) ImportText(c *gin.Context) {
	var req importTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	res, err := h.service.ImportText(req.Source, req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"source_id":     res.SourceID,
		"message_count": res.MessageCount,
	})
}
