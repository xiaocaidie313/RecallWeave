package http

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MountMCP exposes the same read-only memory tools over Streamable HTTP.
// The endpoint is not registered unless the caller provides a bearer token.
func MountMCP(router *gin.Engine, server *mcp.Server, token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	expected := sha256.Sum256([]byte(token))

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	router.Any("/mcp", func(c *gin.Context) {
		scheme, supplied, ok := strings.Cut(c.GetHeader("Authorization"), " ")
		actual := sha256.Sum256([]byte(supplied))
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(c.Writer, c.Request)
	})
}
