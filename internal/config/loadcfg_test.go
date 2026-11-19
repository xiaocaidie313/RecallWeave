package config

import (
	"strings"
	"testing"
)

func TestConfigDefaultsAndEnvironmentOverrides(t *testing.T) {
	t.Setenv("RECALLWEAVE_PORT", "9010")
	t.Setenv("RECALLWEAVE_DB_PASSWORD", "environment-password")
	t.Setenv("RECALLWEAVE_LLM_API_KEY", "environment-key")
	t.Setenv("RECALLWEAVE_EMBEDDING_MODEL", "")
	cfg, err := loadSubagentTestConfig(t, "server:\n  port: 8008\ndatabase:\n  password: file-password\nllm:\n  api_key: file-key\n  embedding_model: file-model\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 9010 || cfg.Database.Port != 5434 || cfg.Database.UserName != "postgres" || cfg.Database.Password != "environment-password" || cfg.LLM.APIKey != "environment-key" || cfg.LLM.EmbeddingModel != "" {
		t.Fatal("configuration did not apply defaults < YAML < explicit environment (including empty strings)")
	}
	t.Setenv("RECALLWEAVE_CONFIG", "custom/config.yaml")
	if ConfigPath() != "custom/config.yaml" {
		t.Fatal("config path override ignored")
	}
}

func TestConfigRejectsInvalidEnvironmentWithoutEchoingValues(t *testing.T) {
	t.Setenv("RECALLWEAVE_PORT", "private-invalid-value")
	_, err := loadSubagentTestConfig(t, "server: {}")
	if err == nil || !strings.Contains(err.Error(), "RECALLWEAVE_PORT") || strings.Contains(err.Error(), "private-invalid-value") {
		t.Fatalf("unexpected environment validation: %v", err)
	}
}

func TestExternalMCPConfiguration(t *testing.T) {
	cfg, err := loadSubagentTestConfig(t, `external_mcps:
  - name: alpha
    url: https://alpha.example/mcp
    token_env: ALPHA_TOKEN
    tools: [search]
  - name: beta
    url: http://127.0.0.1:8090/mcp
    tools: [search]
`)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := cfg.MCPServers()
	if err != nil || len(servers) != 2 || servers[0].Namespace != "alpha" || servers[1].Namespace != "beta" || servers[0].Version != "0.1.0" {
		t.Fatalf("unexpected external server configuration: %+v %v", servers, err)
	}
	legacy, err := loadSubagentTestConfig(t, "external_mcp:\n  url: https://example.com/mcp\n  tools: [search]\n")
	if err != nil {
		t.Fatal(err)
	}
	servers, err = legacy.MCPServers()
	if err != nil || len(servers) != 1 || servers[0].Namespace != "" || servers[0].Name == "" {
		t.Fatal("legacy tool names must remain stable")
	}
}

func TestExternalMCPRejectsAmbiguousOrInvalidConfiguration(t *testing.T) {
	valid := ExternalMCPConfig{Name: "alpha", URL: "https://example.com/mcp", Tools: []string{"search"}}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"mixed configuration", func(c *Config) { c.ExternalMCP = valid }},
		{"duplicate server", func(c *Config) { c.ExternalMCPs = append(c.ExternalMCPs, valid) }},
		{"missing name", func(c *Config) { c.ExternalMCPs[0].Name = "" }},
		{"missing endpoint", func(c *Config) { c.ExternalMCPs[0].URL = "" }},
		{"wrong transport", func(c *Config) { c.ExternalMCPs[0].URL = "stdio://server" }},
		{"URL credentials", func(c *Config) { c.ExternalMCPs[0].URL = "https://user:private-marker@example.com/mcp" }},
		{"URL secret query", func(c *Config) { c.ExternalMCPs[0].URL += "?token=private-marker" }},
		{"literal token", func(c *Config) { c.ExternalMCPs[0].TokenEnv = "secret-private-marker" }},
		{"missing allowlist", func(c *Config) { c.ExternalMCPs[0].Tools = nil }},
		{"duplicate tool", func(c *Config) { c.ExternalMCPs[0].Tools = []string{"search", "search"} }},
		{"invalid tool", func(c *Config) { c.ExternalMCPs[0].Tools = []string{"search.invalid"} }},
		{"long model name", func(c *Config) { c.ExternalMCPs[0].Tools = []string{strings.Repeat("x", 60)} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{ExternalMCPs: []ExternalMCPConfig{valid}}
			test.mutate(cfg)
			_, err := cfg.MCPServers()
			if err == nil || strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("invalid config accepted or secret echoed: %v", err)
			}
		})
	}
}
