package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"recallweave/internal/config"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newFleetTestServer(t *testing.T, label, token string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "same-remote-name", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "search", Description: "Search " + label},
		func(_ context.Context, _ *gomcp.CallToolRequest, input struct {
			Q string `json:"q"`
		}) (*gomcp.CallToolResult, struct {
			Text string `json:"text"`
		}, error) {
			return nil, struct {
				Text string `json:"text"`
			}{Text: label + ":" + input.Q}, nil
		})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server }, &gomcp.StreamableHTTPOptions{JSONResponse: true})
	requests, closed := &atomic.Int32{}, &atomic.Int32{}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "wrong credential", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			closed.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	return httpServer, requests, closed
}

func TestConfiguredFleetNamespacesAndIsolatesCredentials(t *testing.T) {
	t.Setenv("ALPHA_TOKEN", "alpha-secret")
	t.Setenv("BETA_TOKEN", "beta-secret")
	alpha, _, _ := newFleetTestServer(t, "alpha", "alpha-secret")
	beta, _, _ := newFleetTestServer(t, "beta", "beta-secret")
	fleet, err := ConnectConfigured(context.Background(), &config.Config{ExternalMCPs: []config.ExternalMCPConfig{
		{Name: "alpha", URL: alpha.URL, TokenEnv: "ALPHA_TOKEN", Tools: []string{"search"}},
		{Name: "beta", URL: beta.URL, TokenEnv: "BETA_TOKEN", Tools: []string{"search"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer fleet.Close()
	if len(fleet.Tools) != 2 {
		t.Fatalf("expected two tools, got %d", len(fleet.Tools))
	}
	for i, name := range []string{"alpha", "beta"} {
		if fleet.Tools[i].Schema.Name != "mcp__"+name+"__search" {
			t.Fatal("tool identity did not use the configured namespace")
		}
		result, err := fleet.Tools[i].Run(context.Background(), `{"q":"database"}`)
		if err != nil || result != `{"text":"`+name+`:database"}` {
			t.Fatalf("tool routed incorrectly: %s %v", result, err)
		}
	}
}

func TestConfiguredFleetClosesAllSessionsOnDiscoveryFailure(t *testing.T) {
	alpha, _, alphaClosed := newFleetTestServer(t, "alpha", "")
	beta, _, betaClosed := newFleetTestServer(t, "beta", "")
	fleet, err := ConnectConfigured(context.Background(), &config.Config{ExternalMCPs: []config.ExternalMCPConfig{
		{Name: "alpha", URL: alpha.URL, Tools: []string{"search"}},
		{Name: "beta", URL: beta.URL, Tools: []string{"missing"}},
	}})
	if err == nil || fleet != nil || !strings.Contains(err.Error(), "not advertised") {
		t.Fatalf("failed startup returned a partial fleet: %v", err)
	}
	if alphaClosed.Load() != 1 || betaClosed.Load() != 1 {
		t.Fatalf("sessions leaked on startup failure: alpha=%d beta=%d", alphaClosed.Load(), betaClosed.Load())
	}
}

func TestConfiguredFleetChecksTokensBeforeConnecting(t *testing.T) {
	t.Setenv("MISSING_MCP_TOKEN", "")
	alpha, requests, _ := newFleetTestServer(t, "alpha", "")
	_, err := ConnectConfigured(context.Background(), &config.Config{ExternalMCPs: []config.ExternalMCPConfig{
		{Name: "alpha", URL: alpha.URL, Tools: []string{"search"}},
		{Name: "beta", URL: alpha.URL, TokenEnv: "MISSING_MCP_TOKEN", Tools: []string{"search"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "MISSING_MCP_TOKEN") || requests.Load() != 0 {
		t.Fatalf("missing credentials were discovered after connecting: %v requests=%d", err, requests.Load())
	}
}
