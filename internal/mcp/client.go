package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// McpClient owns one connection to an external Streamable HTTP MCP server.
type McpClient struct {
	Client  *gomcp.Client
	Session *gomcp.ClientSession
}

// ConnectHTTP establishes a client session. A token, when provided, is sent
// as a Bearer header; callers should source it from an environment variable.
func ConnectHTTP(ctx context.Context, endpoint, token string) (*McpClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("invalid MCP HTTP endpoint %q", endpoint)
	}
	httpClient := &http.Client{Timeout: 15 * time.Second}
	if token != "" {
		httpClient.Transport = bearerTransport{base: http.DefaultTransport, token: token}
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "recallweave", Version: "0.1.0"}, nil)
	transport := &gomcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to external MCP server: %w", err)
	}
	return &McpClient{Client: client, Session: session}, nil
}

// ListTools collects all pages of remote tools.
func (c *McpClient) ListTools(ctx context.Context) ([]*gomcp.Tool, error) {
	if c == nil || c.Session == nil {
		return nil, fmt.Errorf("MCP client is not connected")
	}
	var tools []*gomcp.Tool
	for tool, err := range c.Session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list external MCP tools: %w", err)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (c *McpClient) Close() error {
	if c == nil || c.Session == nil {
		return nil
	}
	return c.Session.Close()
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

// 效果上相当于 劫持原来的请求 加上自己的 东西 然后再发送 因为 http.Request 是"不可变的" 用的是别人封装的请求 所以需要克隆一个出来 然后加上自己的东西 然后再发送
func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+strings.TrimSpace(t.token))
	return t.base.RoundTrip(copy)
}
