package http

import (
	"errors"
	"io"
	"net/http"
	"recallweave/internal/ingest"
	"recallweave/internal/ingest/parser"
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

type importFileRequest struct {
	SourceTag string `json:"source_tag" binding:"required"`
	Name      string `json:"name"`
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

func (h *IngestHandler) ImportFile(c *gin.Context) {
	var req importFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	// multipart/form-data
	file, err := c.FormFile("file") // 文件指针
	if err != nil {
		return
	}
	// 都是二进制 数据
	content, err := file.Open()
	defer content.Close()
	if err != nil {
		return
	}

	// 读取文件
	data, err := io.ReadAll(content)
	final, err := parser.ReadFile(data)
	if err != nil {
		return
	}
	// 存入数据库？磁盘
	result, err := h.service.ImportText(
		c.Request.Context(),
		store.SourceTag(req.SourceTag),
		req.Name,
		final,
	)

	c.JSON(http.StatusOK, gin.H{
		"upload_file":   "ok",
		"session_id":    result.SessionID,
		"message_count": result.MessageCount,
	})

}
