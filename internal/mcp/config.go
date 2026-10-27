package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 准备接入多个外部mcp

type externalMCPs struct {
	configs []ex_mcp_config
}

type ex_mcp_config struct {
	URL   string       `json:"url,omitempty"`
	Token string       `json:"token,omitempty"`
	Tools []gomcp.Tool `json:"tools,omitempty"`
}

// 通过 json 给格式导入
func InputJsonConfig(raw []byte) (*ex_mcp_config, error) {
	var cfg ex_mcp_config
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

func ConnectExternalMCPs(ctx context.Context, configs []ex_mcp_config) ([]*McpClient, error) {
	clients := make([]*McpClient, 0, len(configs))
	for _, cfg := range configs {
		client, err := ConnectHTTP(ctx, cfg.URL, cfg.Token)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to MCP: %w", err)
		}
		clients = append(clients, client)
	}
	return clients, nil
}
