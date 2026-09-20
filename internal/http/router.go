package http

import (
	"net/http"

	"recallweave/internal/ingest"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func NewRouter(db *gorm.DB) *gin.Engine {
	router := gin.Default()

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	ingestRepo := ingest.NewIngestRepo(db)
	ingestService := ingest.NewIngestService(ingestRepo)
	ingestHandler := ingest.NewIngestHandler(ingestService)

	router.POST("/api/imports", ingestHandler.ImportText)

	return router
}
