package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig        `yaml:"server"`
	Database     DatabaseConfig      `yaml:"database"`
	LLM          LLMConfig           `yaml:"llm"`
	Skills       SkillsConfig        `yaml:"skills"`
	Artifacts    ArtifactsConfig     `yaml:"artifacts"`
	ExternalMCP  ExternalMCPConfig   `yaml:"external_mcp"`
	ExternalMCPs []ExternalMCPConfig `yaml:"external_mcps"`
	Subagents    SubagentsConfig     `yaml:"subagents"`
}

// ExternalMCPConfig describes a Streamable HTTP server and an explicit tool allowlist.
type ExternalMCPConfig struct {
	Name     string   `yaml:"name"`
	Version  string   `yaml:"version"`
	URL      string   `yaml:"url"`
	TokenEnv string   `yaml:"token_env"`
	Tools    []string `yaml:"tools"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
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
	cfg, _ := LoadDefaultConfig()
	// 获得二进制流 文件 ----> 待 转化为配置文件
	cfgFile, err := os.ReadFile(configPath)
	if err != nil {
		return &Config{}, fmt.Errorf("failed to read config file: %w", err)
	}
	if err := yaml.Unmarshal(cfgFile, cfg); err != nil {
		return &Config{}, fmt.Errorf("failed to unmarshal config file: %w", err)
	}
	if err := cfg.applyEnvironment(); err != nil {
		return &Config{}, err
	}
	if err := cfg.Subagents.Validate(); err != nil {
		return &Config{}, fmt.Errorf("invalid subagent configuration: %w", err)
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return &Config{}, fmt.Errorf("server.port must be between 1 and 65535")
	}
	if cfg.Database.Port < 1 || cfg.Database.Port > 65535 {
		return &Config{}, fmt.Errorf("database.port must be between 1 and 65535")
	}
	if cfg.LLM.TimeoutSeconds < 0 || cfg.LLM.TimeoutSeconds > 3600 {
		return &Config{}, fmt.Errorf("llm.timeout_seconds must be between 0 and 3600")
	}
	if _, err := cfg.MCPServers(); err != nil {
		return &Config{}, err
	}
	return cfg, nil
}

// 环境默认配置
func LoadDefaultConfig() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 8008},
		Database: DatabaseConfig{
			Host:     "localhost",
			Port:     5434,
			UserName: "postgres",
			DBName:   "recallweave",
		},
		Skills: SkillsConfig{Dir: "skills"},
		LLM:    LLMConfig{TimeoutSeconds: 30},
	}
	return cfg, nil
}

// ConfigPath is shared by the HTTP and stdio entry points; -config overrides it.
func ConfigPath() string {
	if path := os.Getenv("RECALLWEAVE_CONFIG"); path != "" {
		return path
	}
	return "config/config.yaml"
}

func (c *Config) applyEnvironment() error {
	for name, field := range map[string]*string{
		"RECALLWEAVE_HOST":            &c.Server.Host,
		"RECALLWEAVE_DB_HOST":         &c.Database.Host,
		"RECALLWEAVE_DB_USER":         &c.Database.UserName,
		"RECALLWEAVE_DB_PASSWORD":     &c.Database.Password,
		"RECALLWEAVE_DB_NAME":         &c.Database.DBName,
		"RECALLWEAVE_LLM_BASE_URL":    &c.LLM.BaseURL,
		"RECALLWEAVE_LLM_API_KEY":     &c.LLM.APIKey,
		"RECALLWEAVE_LLM_MODEL":       &c.LLM.Model,
		"RECALLWEAVE_EMBEDDING_MODEL": &c.LLM.EmbeddingModel,
	} {
		if value, ok := os.LookupEnv(name); ok {
			*field = value
		}
	}
	for name, field := range map[string]*int{
		"RECALLWEAVE_PORT":                &c.Server.Port,
		"RECALLWEAVE_DB_PORT":             &c.Database.Port,
		"RECALLWEAVE_LLM_TIMEOUT_SECONDS": &c.LLM.TimeoutSeconds,
	} {
		if value, ok := os.LookupEnv(name); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%s must be an integer", name)
			}
			*field = parsed
		}
	}
	return nil
}
