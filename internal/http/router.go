package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter 只做「路径 → 处理函数」的映射，依赖在 main 里组装好再传进来。
func NewRouter(
	ingestHandler *IngestHandler,
	extractHandler *ExtractHandler,
	askHandler *AskHandler,
	conversationHandler *ConversationHandler,
) *gin.Engine {
	router := gin.Default()

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	router.POST("/api/imports", ingestHandler.ImportText)
	router.POST("/api/imports/file", ingestHandler.ImportFile)
	router.GET("/api/sessions/:session_id", conversationHandler.GetSession)
	router.GET("/api/sessions/:session_id/memories", conversationHandler.ListSessionMemories)
	router.GET("/api/sessions/:session_id/messages", conversationHandler.ListSessionMessages)
	router.POST("/api/sessions/:session_id/extract", extractHandler.ExtractSession)
	router.POST("/api/ask/:conversation_id", askHandler.Ask)

	ConversationGroup := router.Group("/api/conversations")
	{
		ConversationGroup.GET("/list", conversationHandler.List)
		ConversationGroup.POST("/create", conversationHandler.Create)
		ConversationGroup.GET("/list/with_title", conversationHandler.ListWithTitle)
		ConversationGroup.GET("/:conversation_id/messages", conversationHandler.ListConversationMessages)
		ConversationGroup.POST("/:conversation_id/imports", ingestHandler.ImportConversation)
		ConversationGroup.POST("/:conversation_id/imports/file", ingestHandler.ImportConversationFile)
	}

	return router
}
