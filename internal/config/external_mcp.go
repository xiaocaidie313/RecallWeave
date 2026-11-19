package config

import (
	"fmt"
	"net/url"
	"regexp"
)

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var mcpToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,60}$`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MCPServer is a validated connection. Legacy configuration keeps mcp_<tool>
// names; list entries use mcp__<namespace>__<tool> to isolate remote identities.
type MCPServer struct {
	ExternalMCPConfig
	Namespace string
}

func (c *Config) MCPServers() ([]MCPServer, error) {
	if c.ExternalMCP.URL != "" && len(c.ExternalMCPs) != 0 {
		return nil, fmt.Errorf("external_mcp and external_mcps cannot be enabled together")
	}
	servers := make([]MCPServer, 0, len(c.ExternalMCPs)+1)
	if c.ExternalMCP.URL != "" {
		legacy := c.ExternalMCP
		if legacy.Name == "" {
			legacy.Name = "recallweave-external"
		}
		servers = append(servers, MCPServer{ExternalMCPConfig: legacy})
	}
	for _, entry := range c.ExternalMCPs {
		servers = append(servers, MCPServer{ExternalMCPConfig: entry, Namespace: entry.Name})
	}
	seen := make(map[string]bool, len(servers))
	for i := range servers {
		s := &servers[i]
		path := fmt.Sprintf("external_mcps[%d]", i)
		if c.ExternalMCP.URL != "" {
			path = "external_mcp"
		}
		if !mcpServerName.MatchString(s.Name) || seen[s.Name] {
			return nil, fmt.Errorf("%s.name must be unique and match %s", path, mcpServerName)
		}
		seen[s.Name] = true
		endpoint, err := url.Parse(s.URL)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawQuery != "" {
			return nil, fmt.Errorf("%s.url must be an HTTP(S) endpoint without credentials, query, or fragment", path)
		}
		if s.TokenEnv != "" && !environmentName.MatchString(s.TokenEnv) {
			return nil, fmt.Errorf("%s.token_env must be an environment variable name", path)
		}
		if len(s.Tools) == 0 {
			return nil, fmt.Errorf("%s.tools must contain an explicit allowlist", path)
		}
		seenTools := make(map[string]bool, len(s.Tools))
		for _, tool := range s.Tools {
			localName := "mcp_" + tool
			if s.Namespace != "" {
				localName = "mcp__" + s.Namespace + "__" + tool
			}
			if !mcpToolName.MatchString(tool) || seenTools[tool] || len(localName) > 64 {
				return nil, fmt.Errorf("%s.tools must be unique valid names with model-facing names of at most 64 characters", path)
			}
			seenTools[tool] = true
		}
		if s.Version == "" {
			s.Version = "0.1.0"
		}
	}
	return servers, nil
}
