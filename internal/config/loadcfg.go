package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
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
	}
	return cfg, nil
}
