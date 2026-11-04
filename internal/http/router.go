package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter 只做「路径 → 处理函数」的映射，依赖在 main 里组装好再传进来。
func NewRouter(
	ingestHandler *IngestHandler,
	extractHandler *ExtractHandler,
	chatHandler *ChatHandler,
	conversationHandler *ConversationHandler,
) *gin.Engine {
	router := gin.Default()

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	if ingestHandler != nil {
		router.POST("/api/imports", ingestHandler.ImportText)
		router.POST("/api/imports/file", ingestHandler.ImportFile)
		router.POST("/api/conversations/:conversation_id/imports", ingestHandler.ImportConversation)
		router.POST("/api/conversations/:conversation_id/imports/file", ingestHandler.ImportConversationFile)
	}
	if extractHandler != nil {
		router.POST("/api/sessions/:session_id/extract", extractHandler.ExtractSession)
	}
	if chatHandler != nil {
		router.POST("/api/chat/:conversation_id", chatHandler.Chat)
	}
	if conversationHandler != nil {
		router.GET("/api/sessions/:session_id", conversationHandler.GetSession)
		router.GET("/api/sessions/:session_id/memories", conversationHandler.ListSessionMemories)
		router.GET("/api/sessions/:session_id/messages", conversationHandler.ListSessionMessages)
		router.GET("/api/runs/:run_id", conversationHandler.GetRun)
		ConversationGroup := router.Group("/api/conversations")
		{
			ConversationGroup.GET("/list", conversationHandler.List)
			ConversationGroup.POST("/create", conversationHandler.Create)
			ConversationGroup.POST("/:conversation_id/fork", conversationHandler.Fork)
			ConversationGroup.GET("/list/with_title", conversationHandler.ListWithTitle)
			ConversationGroup.GET("/:conversation_id/messages", conversationHandler.ListConversationMessages)
			ConversationGroup.GET("/:conversation_id/runs", conversationHandler.ListRuns)
		}
	}

	return router
}
