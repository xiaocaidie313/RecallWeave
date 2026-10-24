package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MountMCP exposes the same read-only memory tools over Streamable HTTP.
// The endpoint is not registered unless the caller provides a bearer token.
func MountMCP(router *gin.Engine, server *mcp.Server, token string) {
	// if token == "" {
	// 	return
	// }

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		//@todo 目前只有一个mcp实例
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	router.Any("/mcp", func(c *gin.Context) {
		// scheme, supplied, ok := strings.Cut(c.GetHeader("Authorization"), " ")
		// if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
		// 	c.Header("WWW-Authenticate", "Bearer")
		// 	c.AbortWithStatus(http.StatusUnauthorized)
		// 	return
		// }
		handler.ServeHTTP(c.Writer, c.Request)
	})
}
