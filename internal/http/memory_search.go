package http

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
)

type MemoryFinder interface {
	FindMemories(context.Context, string, types.MemoryTag, int, ...bool) ([]types.Memory, error)
}

// MountMemorySearch exposes retrieval independently of a chat model or MCP client.
func MountMemorySearch(router *gin.Engine, finder MemoryFinder) {
	router.GET("/api/memories/search", func(c *gin.Context) {
		query := strings.TrimSpace(c.Query("q"))
		if query == "" || len(query) > 4096 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "q must contain 1 to 4096 bytes of text"})
			return
		}
		limit := 5
		if raw, exists := c.GetQuery("limit"); exists {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 20 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 20"})
				return
			}
			limit = value
		}
		tag := types.MemoryTag(c.Query("tag"))
		if tag != "" && types.NormalizeMemoryTag(string(tag)) != tag {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown memory tag"})
			return
		}
		inactive := c.Query("include_inactive")
		if inactive != "" && inactive != "true" && inactive != "false" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "include_inactive must be true or false"})
			return
		}
		if finder == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory search is unavailable"})
			return
		}
		items, err := finder.FindMemories(c.Request.Context(), query, tag, limit, inactive == "true")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search memories"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"memories": memoryViews(items)})
	})
}
