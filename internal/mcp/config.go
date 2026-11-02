package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 准备接入多个外部mcp

type externalMCPs struct {
	configs []MCPServerConfig
}

type MCPServerConfig struct {
	Name    string            `json:"name,omitempty"`
	Version string            `json:"version,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tools   []mcp.Tool        `json:"tools,omitempty"`
	Args    []string          `json:"args,omitempty"`
}

// 通过 json 给格式导入
func InputJsonConfig(raw []byte) (*MCPServerConfig, error) {
	var cfg MCPServerConfig
	str_cfg := strings.TrimSpace(string(raw))
	if err := json.Unmarshal([]byte(str_cfg), &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	return &cfg, nil
}

func (e *externalMCPs) AddNewExternalMCP(raw []byte) error {
	cfg, err := InputJsonConfig(raw)
	if err != nil {
		return fmt.Errorf("failed to add MCP: %w", err)
	}
	e.configs = append(e.configs, *cfg)
	return nil
}

func ConnectExternalMCPs(ctx context.Context, configs []MCPServerConfig) ([]*McpClient, error) {
	clients := make([]*McpClient, 0, len(configs))
	for _, cfg := range configs {
		if strings.TrimSpace(cfg.URL) == "" {
			continue
		}
		client, err := NewRemoteClient(ctx, cfg.Name, cfg.Version, cfg.URL, cfg.Headers)
		if err != nil {
			for _, connected := range clients {
				_ = connected.Close()
			}
			return nil, fmt.Errorf("failed to connect to MCP: %w", err)
		}
		clients = append(clients, client)
	}
	return clients, nil
}

func parseMCPHeaders(value string) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return nil, fmt.Errorf("failed to unmarshal headers: %w", err)
	}
	for k, v := range m {
		if k == "" {
			return nil, fmt.Errorf("empty header key")
		}
		if v == "" {
			return nil, fmt.Errorf("empty header key")
		}
	}
	return m, nil
}
