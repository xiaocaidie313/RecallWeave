package http

import (
	"errors"
	"io"
	"net/http"
	"recallweave/internal/ingest"
	"recallweave/internal/parser"
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

	results, err := h.service.ImportText(
		c.Request.Context(),
		store.SourceTag(req.SourceTag),
		req.Name,
		req.Text,
	)
	if err != nil {
		writeImportError(c, err)
		return
	}
	writeImportResults(c, results)
}

func (h *IngestHandler) ImportFile(c *gin.Context) {
	sourceTag := c.PostForm("source_tag")
	if sourceTag == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source_tag is required"})
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}

	content, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer content.Close()

	data, err := io.ReadAll(io.LimitReader(content, 20<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	text, err := parser.ReadFile(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	results, err := h.service.ImportText(
		c.Request.Context(),
		store.SourceTag(sourceTag),
		c.PostForm("name"),
		text,
	)
	if err != nil {
		writeImportError(c, err)
		return
	}
	writeImportResults(c, results)
}

func writeImportError(c *gin.Context, err error) {
	if errors.Is(err, ingest.ErrInvalidSource) || errors.Is(err, ingest.ErrEmptyText) || errors.Is(err, ingest.ErrUnreadable) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func writeImportResults(c *gin.Context, results []ingest.ImportResult) {
	sessions := make([]gin.H, 0, len(results))
	for _, result := range results {
		sessions = append(sessions, gin.H{
			"session_id":    result.SessionID,
			"message_count": result.MessageCount,
		})
	}
	c.JSON(http.StatusCreated, gin.H{"sessions": sessions})
}
