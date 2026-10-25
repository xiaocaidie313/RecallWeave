package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(copy)
}

func TestMountMCPWithoutToken(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	router := NewRouter(nil, nil, nil, nil)
	MountMCP(router, server, "")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("stateless MCP GET should return 405, got %d", response.Code)
	}
}

func TestMCPOverStreamableHTTP(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct {
		OK bool `json:"ok"`
	}, error) {
		return nil, struct {
			OK bool `json:"ok"`
		}{OK: true}, nil
	})
	router := NewRouter(nil, nil, nil, nil)
	MountMCP(router, server, "secret")
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: "secret"}},
		DisableStandaloneSSE: true,
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect MCP over HTTP: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "ping" {
		t.Fatalf("list tools: result=%+v err=%v", tools, err)
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping"})
	if err != nil || result.IsError {
		t.Fatalf("call tool: result=%+v err=%v", result, err)
	}
}
