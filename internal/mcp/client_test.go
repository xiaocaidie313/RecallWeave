package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConnectHTTPListsTools(t *testing.T) {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "external-test", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "ping", Description: "Check connectivity"},
		func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()

	client, err := ConnectHTTP(context.Background(), httpServer.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" || tools[0].Description != "Check connectivity" {
		t.Fatalf("unexpected remote tools: %+v", tools)
	}
}

func TestConnectHTTPRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "stdio://server", "https://user:pass@example.com/mcp", "not a URL"} {
		if _, err := ConnectHTTP(context.Background(), endpoint, ""); err == nil {
			t.Errorf("expected %q to be rejected", endpoint)
		}
	}
	var client *McpClient
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Fatal("disconnected client should not list tools")
	}
}
