package extract

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type ExtractHandler struct {
	excuter *ExtractExcuter
}

func NewExtractHandler(excuter *ExtractExcuter) *ExtractHandler {
	return &ExtractHandler{excuter: excuter}
}

// ExtractSource 手动触发提炼，目前只用来验证链路。
// 等提炼稳定后，这一步会由导入成功之后自动带起来。
func (h *ExtractHandler) ExtractSource(c *gin.Context) {
	sourceID, err := strconv.ParseUint(c.Param("source_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid source id",
		})
		return
	}

	count, err := h.excuter.ExtractSource(uint(sourceID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"source_id":    sourceID,
		"memory_count": count,
	})
}
