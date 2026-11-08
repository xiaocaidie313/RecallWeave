package http

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"recallweave/internal/ingest"
	"recallweave/internal/parser"
	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
)

type IngestHandler struct {
	service *ingest.Service
}

func NewIngestHandler(service *ingest.Service) *IngestHandler {
	return &IngestHandler{service: service}
}

type importTextRequest struct {
	SourceTag string        `json:"source_tag" binding:"required"`
	Name      string        `json:"name"`
	Text      string        `json:"text" binding:"required"`
	Format    parser.Format `json:"format"`
}

// ImportText 导入知识背景。不挂对话，记忆进全库，以后按问题相似度被捞到。
func (h *IngestHandler) ImportText(c *gin.Context) {
	var req importTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	results, err := h.service.ImportTextWithFormat(
		c.Request.Context(),
		types.SourceTag(req.SourceTag),
		req.Name,
		req.Text,
		req.Format,
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

	text, format, err := readUploadedText(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	results, err := h.service.ImportTextWithFormat(
		c.Request.Context(),
		types.SourceTag(sourceTag),
		c.PostForm("name"),
		text,
		format,
	)
	if err != nil {
		writeImportError(c, err)
		return
	}
	writeImportResults(c, results)
}

type importConversationRequest struct {
	SourceTag string        `json:"source_tag" binding:"required"`
	Name      string        `json:"name"`
	Text      string        `json:"text" binding:"required"`
	Format    parser.Format `json:"format"`
}

// ImportConversation 把文本挂到已有对话上，作为这次对话的 context。
func (h *IngestHandler) ImportConversation(c *gin.Context) {
	conversationID, ok := conversationIDFromPath(c)
	if !ok {
		return
	}

	var req importConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	results, err := h.service.ImportIntoConversationWithFormat(
		c.Request.Context(),
		conversationID,
		types.SourceTag(req.SourceTag),
		req.Name,
		req.Text,
		req.Format,
	)
	if err != nil {
		writeImportError(c, err)
		return
	}
	writeImportResults(c, results)
}

func (h *IngestHandler) ImportConversationFile(c *gin.Context) {
	conversationID, ok := conversationIDFromPath(c)
	if !ok {
		return
	}

	sourceTag := c.PostForm("source_tag")
	if sourceTag == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source_tag is required"})
		return
	}

	text, format, err := readUploadedText(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	results, err := h.service.ImportIntoConversationWithFormat(
		c.Request.Context(),
		conversationID,
		types.SourceTag(sourceTag),
		c.PostForm("name"),
		text,
		format,
	)
	if err != nil {
		writeImportError(c, err)
		return
	}
	writeImportResults(c, results)
}

func conversationIDFromPath(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("conversation_id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return 0, false
	}
	return uint(id), true
}

func readUploadedText(c *gin.Context) (string, parser.Format, error) {
	format := parser.Format(c.PostForm("format"))
	if !format.Valid() {
		return "", parser.FormatAuto, ingest.ErrInvalidFormat
	}
	file, err := c.FormFile("file")
	if err != nil {
		return "", parser.FormatAuto, errors.New("file is required")
	}

	content, err := file.Open()
	if err != nil {
		return "", parser.FormatAuto, err
	}
	defer content.Close()

	data, err := io.ReadAll(io.LimitReader(content, 20<<20))
	if err != nil {
		return "", parser.FormatAuto, err
	}
	text, detected, err := parser.ReadFileWithFormat(data, file.Filename)
	if err != nil {
		return "", parser.FormatAuto, err
	}
	if format != parser.FormatAuto {
		detected = format
	}
	return text, detected, nil
}

func writeImportError(c *gin.Context, err error) {
	if errors.Is(err, ingest.ErrConversationNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, ingest.ErrInvalidSource) || errors.Is(err, ingest.ErrInvalidFormat) || errors.Is(err, ingest.ErrEmptyText) || errors.Is(err, ingest.ErrUnreadable) || errors.Is(err, ingest.ErrChatSource) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func writeImportResults(c *gin.Context, results []ingest.ImportResult) {
	sessions := make([]gin.H, 0, len(results))
	for _, result := range results {
		sessions = append(sessions, gin.H{
			"session_id":     result.SessionID,
			"message_count":  result.MessageCount,
			"extract_status": result.ExtractStatus,
		})
	}
	c.JSON(http.StatusCreated, gin.H{"sessions": sessions})
}
