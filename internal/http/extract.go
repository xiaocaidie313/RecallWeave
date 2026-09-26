package http

import (
	"net/http"
	"strconv"

	"recallweave/internal/extract"

	"github.com/gin-gonic/gin"
)

type ExtractHandler struct {
	excuter *extract.ExtractExcuter
}

func NewExtractHandler(excuter *extract.ExtractExcuter) *ExtractHandler {
	return &ExtractHandler{excuter: excuter}
}

// ExtractSession 手动触发提炼，目前只用来验证链路。
func (h *ExtractHandler) ExtractSession(c *gin.Context) {
	sessionID, err := strconv.ParseUint(c.Param("session_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid session id",
		})
		return
	}

	count, err := h.excuter.ExtractSession(c.Request.Context(), uint(sessionID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"session_id":   sessionID,
		"memory_count": count,
	})
}
