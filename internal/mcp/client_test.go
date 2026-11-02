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

	client, err := NewRemoteClient(context.Background(), "external-test", "1.0.0", httpServer.URL, map[string]string{
		"Authorization": "Bearer secret",
	})
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
		if _, err := NewRemoteClient(context.Background(), "external-test", "1.0.0", endpoint, nil); err == nil {
			t.Errorf("expected %q to be rejected", endpoint)
		}
	}
	var client *McpClient
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Fatal("disconnected client should not list tools")
	}
}

func TestConnectDialsEveryConfig(t *testing.T) {
	ping := func(name string) *httptest.Server {
		server := gomcp.NewServer(&gomcp.Implementation{Name: name, Version: "1.0.0"}, nil)
		gomcp.AddTool(server, &gomcp.Tool{Name: name},
			func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
				return nil, struct{}{}, nil
			})
		handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server },
			&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		return httptest.NewServer(handler)
	}
	first := ping("alpha")
	defer first.Close()
	second := ping("beta")
	defer second.Close()

	alpha, err := NewRemoteClient(context.Background(), "external-test", "1.0.0", first.URL, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	defer alpha.Close()
	beta, err := NewRemoteClient(context.Background(), "external-test", "1.0.0", second.URL, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	defer beta.Close()
	if len(alpha.Tools) != 1 || alpha.Tools[0].Name != "alpha" {
		t.Fatalf("alpha tools: %+v", alpha.Tools)
	}
	if len(beta.Tools) != 1 || beta.Tools[0].Name != "beta" {
		t.Fatalf("beta tools: %+v", beta.Tools)
	}
}

func TestConnectSkipsEmptyURLs(t *testing.T) {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "keep", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "keep"},
		func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	clients, err := ConnectExternalMCPs(context.Background(), []MCPServerConfig{
		{Name: "external-test", Version: "1.0.0", URL: ""},
		{Name: "external-test", Version: "1.0.0", URL: "   "},
		{Name: "keep", Version: "1.0.0", URL: httpServer.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 {
		t.Fatalf("expected the empty URLs to be skipped, got %d clients", len(clients))
	}
	defer clients[0].Close()
	if len(clients[0].Tools) != 1 || clients[0].Tools[0].Name != "keep" {
		t.Fatalf("keep tools: %+v", clients[0].Tools)
	}
}
