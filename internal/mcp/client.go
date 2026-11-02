package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	net "net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// McpClient owns connections to external Streamable HTTP MCP servers.
// Client/Session are the first successful connection; others holds the rest.
type McpClient struct {
	name    string
	Client  *mcp.Client
	Session *mcp.ClientSession
	Tools   []*mcp.Tool
}

// ConnectHTTP establishes a client session. A token, when provided, is sent
// as a Bearer header; callers should source it from an environment variable.
func NewRemoteClient(ctx context.Context, name, version, url string, headers map[string]string) (*McpClient, error) {
	parsed, err := net.Parse(url) // 解析url
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("invalid MCP HTTP url %q", url)
	}
	httpClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: headerTransport{
			base:       http.DefaultTransport,
			headers:    headers,
			serverName: name,
		},
	}

	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: version}, nil)

	transport := &mcp.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to external MCP server: %w", err)
	}

	toolResult, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}

	var success bool
	success = false
	defer func() {
		if !success {
			session.Close()
		}
	}()

	success = true
	return &McpClient{
		name:    name,
		Client:  client,
		Session: session,
		Tools:   toolResult.Tools,
	}, nil
}
func (c *McpClient) Name() string { return c.name }

// ListTools collects all pages of remote tools.
func (c *McpClient) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	if c == nil || c.Session == nil {
		return nil, fmt.Errorf("MCP client is not connected")
	}
	var tools []*mcp.Tool
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

// 实现 http.RoundTripper 接口 真实请求调用这个接口
type headerTransport struct {
	base       http.RoundTripper
	headers    map[string]string
	serverName string
}

// 效果上相当于 劫持原来的请求 加上自己的 东西 然后再发送 因为 http.Request 是"不可变的" 用的是别人封装的请求 所以需要克隆一个出来 然后加上自己的东西 然后再发送
func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	for k, v := range t.headers {
		copy.Header.Set(k, v)
	}
	resp, err := t.base.RoundTrip(copy)
	if err != nil {
		return nil, err
	}
	// 请求的mcp的返回码
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("remote MCP server %q returned HTTP 401 Unauthorized — check your token/header configuration", t.serverName)
	case http.StatusForbidden:
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("remote MCP server %q returned HTTP 403 Forbidden — your credentials may lack required permissions", t.serverName)
	}
	return resp, nil
}
