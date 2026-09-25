package http

import (
	"net/http"

	"recallweave/internal/ask"
	"recallweave/internal/extract"
	"recallweave/internal/ingest"

	"github.com/gin-gonic/gin"
)

// NewRouter 只做「路径 → 处理函数」的映射，依赖在 main 里组装好再传进来。
func NewRouter(
	ingestHandler *ingest.IngestHandler,
	extractHandler *extract.ExtractHandler,
	askHandler *ask.AskHandler,
) *gin.Engine {
	router := gin.Default()

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	router.POST("/api/imports", ingestHandler.ImportText)
	router.POST("/api/sessions/:session_id/extract", extractHandler.ExtractSession)
	router.POST("/api/ask/:conversation_id", askHandler.Ask)

	return router
}
