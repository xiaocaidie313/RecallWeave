package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAgentToolsAllowlistAndRemoteCall(t *testing.T) {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "external-test", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "ping", Description: "Check connectivity"},
		func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct {
			OK bool `json:"ok"`
		}, error) {
			return nil, struct {
				OK bool `json:"ok"`
			}{OK: true}, nil
		})
	gomcp.AddTool(server, &gomcp.Tool{Name: "fail"},
		func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, errors.New("remote failure")
		})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	client, err := ConnectHTTP(context.Background(), httpServer.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	tools, err := client.AgentTools(context.Background(), []string{"ping"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Schema.Name != "mcp_ping" || tools[0].Schema.Description != "Check connectivity" {
		t.Fatalf("unexpected external tools: %+v", tools)
	}
	if got, err := tools[0].Run(context.Background(), `{}`); err != nil || got != `{"ok":true}` {
		t.Fatalf("unexpected remote output %q, err=%v", got, err)
	}
	if _, err := tools[0].Run(context.Background(), `invalid`); err == nil {
		t.Fatal("invalid tool arguments should fail")
	}
	for _, names := range [][]string{{}, {"missing"}, {"ping", "ping"}} {
		if _, err := client.AgentTools(context.Background(), names); err == nil {
			t.Errorf("expected allowlist %v to fail", names)
		}
	}

	failing, err := client.AgentTools(context.Background(), []string{"fail"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing[0].Run(context.Background(), `{}`); err == nil || !strings.Contains(err.Error(), "remote failure") {
		t.Fatalf("remote isError should become a tool error, got %v", err)
	}
}
