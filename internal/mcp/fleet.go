package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"recallweave/internal/config"
	"recallweave/internal/llm"
)

// Fleet owns all external sessions. Startup is atomic: a failed connection or
// tool allowlist closes every previously opened session and returns no tools.
type Fleet struct {
	clients []*McpClient
	Tools   []llm.Tool
}

func ConnectConfigured(ctx context.Context, cfg *config.Config) (*Fleet, error) {
	servers, err := cfg.MCPServers()
	if err != nil {
		return nil, err
	}
	// Resolve every credential before opening any network connections.
	headers := make([]map[string]string, len(servers))
	for i, server := range servers {
		if server.TokenEnv == "" {
			continue
		}
		token := strings.TrimSpace(os.Getenv(server.TokenEnv))
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, fmt.Errorf("external MCP %q requires a valid token in environment variable %s", server.Name, server.TokenEnv)
		}
		headers[i] = map[string]string{"Authorization": "Bearer " + token}
	}
	fleet := &Fleet{}
	success := false
	defer func() {
		if !success {
			_ = fleet.Close()
		}
	}()
	seen := make(map[string]bool)
	for i, server := range servers {
		connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		client, err := NewRemoteClient(connectCtx, server.Name, server.Version, server.URL, headers[i])
		if err != nil {
			cancel()
			return nil, fmt.Errorf("external MCP %q: %w", server.Name, err)
		}
		fleet.clients = append(fleet.clients, client)
		remoteTools, err := client.NamespacedAgentTools(connectCtx, server.Namespace, server.Tools)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("external MCP %q: %w", server.Name, err)
		}
		for _, tool := range remoteTools {
			if seen[tool.Schema.Name] {
				return nil, fmt.Errorf("duplicate external MCP tool name %q", tool.Schema.Name)
			}
			seen[tool.Schema.Name] = true
		}
		fleet.Tools = append(fleet.Tools, remoteTools...)
	}
	success = true
	return fleet, nil
}

func (f *Fleet) Close() error {
	if f == nil {
		return nil
	}
	var errs []error
	for i := len(f.clients) - 1; i >= 0; i-- {
		errs = append(errs, f.clients[i].Close())
	}
	f.clients = nil
	return errors.Join(errs...)
}
