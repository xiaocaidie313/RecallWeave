package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Database    DatabaseConfig    `yaml:"database"`
	LLM         LLMConfig         `yaml:"llm"`
	Skills      SkillsConfig      `yaml:"skills"`
	Artifacts   ArtifactsConfig   `yaml:"artifacts"`
	ExternalMCP ExternalMCPConfig `yaml:"external_mcp"`
	Subagents   SubagentsConfig   `yaml:"subagents"`
}

// ExternalMCPConfig enables one remote Streamable HTTP MCP server. Empty URL
// disables it; Tools is an explicit allowlist of remote tool names.
type ExternalMCPConfig struct {
	URL      string   `yaml:"url"`
	TokenEnv string   `yaml:"token_env"`
	Tools    []string `yaml:"tools"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	UserName string `yaml:"user_name"`
	Password string `yaml:"password"`
	DBName   string `yaml:"db_name"`
}

// LLMConfig 留空时提炼会退回到本地兜底，不调模型。
type LLMConfig struct {
	BaseURL        string `yaml:"base_url"`
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	EmbeddingModel string `yaml:"embedding_model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// SkillsConfig points at the local Skill catalog. Empty Dir means "skills".
type SkillsConfig struct {
	Dir string `yaml:"dir"`
}

type ArtifactsConfig struct {
	Dir string `yaml:"dir"`
}

// ArtifactDir follows the same environment/config/default precedence as skills.
func (c *Config) ArtifactDir() string {
	if dir := os.Getenv("RECALLWEAVE_ARTIFACTS_DIR"); dir != "" {
		return dir
	}
	if c != nil && c.Artifacts.Dir != "" {
		return c.Artifacts.Dir
	}
	return "data/artifacts"
}

// SkillDir returns the Skill root: env override, then config, then "skills".
func (c *Config) SkillDir() string {
	if dir := os.Getenv("RECALLWEAVE_SKILLS_DIR"); dir != "" {
		return dir
	}
	if c != nil && c.Skills.Dir != "" {
		return c.Skills.Dir
	}
	return "skills"
}

// 自定义  通过加载配置文件
func LoadConfig(configPath string) (*Config, error) {
	var cfg Config
	// 获得二进制流 文件 ----> 待 转化为配置文件
	cfgFile, err := os.ReadFile(configPath)
	if err != nil {
		return &Config{}, fmt.Errorf("failed to read config file: %w", err)
	}
	if err := yaml.Unmarshal(cfgFile, &cfg); err != nil {
		return &Config{}, fmt.Errorf("failed to unmarshal config file: %w", err)
	}
	if err := cfg.Subagents.Validate(); err != nil {
		return &Config{}, fmt.Errorf("invalid subagent configuration: %w", err)
	}
	return &cfg, nil
}

// 环境默认配置
func LoadDefaultConfig() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{Port: 8008},
		Database: DatabaseConfig{
			Host:     "localhost",
			Port:     3306,
			UserName: "root",
			Password: "123456",
			DBName:   "recallweave",
		},
		Skills: SkillsConfig{Dir: "skills"},
	}
	return cfg, nil
}
