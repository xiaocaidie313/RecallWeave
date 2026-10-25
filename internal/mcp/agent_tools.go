package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"recallweave/internal/llm"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxRemoteResultBytes = 64 << 10

var validAgentToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,60}$`)

// AgentTools discovers remote tools once and wraps only the explicitly allowed
// names as local Agent tools. The returned closures use this client's session.
func (c *McpClient) AgentTools(ctx context.Context, allowed []string) ([]llm.Tool, error) {
	if len(allowed) == 0 {
		return nil, fmt.Errorf("external MCP tools allowlist is empty")
	}
	remote, err := c.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*gomcp.Tool, len(remote))
	for _, tool := range remote {
		// 根据名字找tool
		byName[tool.Name] = tool
	}
	seen := make(map[string]bool)
	wrapped := make([]llm.Tool, 0, len(allowed))
	for _, name := range allowed {
		if !validAgentToolName.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate external MCP tool name %q", name)
		}
		seen[name] = true
		tool, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("external MCP tool %q was not advertised", name)
		}
		parameters, ok := tool.InputSchema.(map[string]any)
		if !ok || parameters["type"] != "object" {
			return nil, fmt.Errorf("external MCP tool %q has unsupported input schema", name)
		}
		remoteName := name
		wrapped = append(wrapped, llm.Tool{
			Schema: llm.ToolSchema{
				Name:        "mcp_" + remoteName,
				Description: tool.Description,
				Parameters:  parameters,
			},
			Run: func(ctx context.Context, arguments string) (string, error) {
				var args map[string]any
				if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
					return "", fmt.Errorf("invalid arguments for external MCP tool %q", remoteName)
				}
				result, err := c.Session.CallTool(ctx, &gomcp.CallToolParams{Name: remoteName, Arguments: args})
				if err != nil {
					return "", fmt.Errorf("external MCP tool %q: %w", remoteName, err)
				}
				content, err := formatMCPResult(result)
				if err != nil {
					return "", fmt.Errorf("external MCP tool %q: %w", remoteName, err)
				}
				return content, nil
			},
		})
	}
	return wrapped, nil
}

func formatMCPResult(result *gomcp.CallToolResult) (string, error) {
	if result == nil {
		return "", fmt.Errorf("empty result")
	}
	var content string
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return "", err
		}
		content = string(encoded)
	} else {
		// text content
		var texts []string
		for _, block := range result.Content {
			if item, ok := block.(*gomcp.TextContent); ok {
				texts = append(texts, item.Text)
			}
		}
		content = strings.Join(texts, "\n")
	}
	if len(content) > maxRemoteResultBytes {
		return "", fmt.Errorf("result exceeds %d bytes", maxRemoteResultBytes)
	}
	if result.IsError {
		if content == "" {
			content = "remote tool reported an error"
		}
		return "", fmt.Errorf("%s", content)
	}
	if content == "" {
		return "", fmt.Errorf("no text or structured content in result")
	}
	return content, nil
}
