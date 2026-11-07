// Command mcp exposes RecallWeave memories to local MCP clients over stdio.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"recallweave/internal/config"
	"recallweave/internal/db"
	"recallweave/internal/llm"
	appmcp "recallweave/internal/mcp"
	"recallweave/internal/memory"
	"recallweave/internal/retrieval"
	"recallweave/internal/session"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 启mcp Server 用暴露 自身mcp工具 @todo？ 是否要单独提出成外部mcp 不放在这个agent仓库
func main() {
	configPath := flag.String("config", "config/config.yaml", "path to RecallWeave configuration")
	flag.Parse()
	// MCP protocol messages use stdout; all diagnostics must use stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(context.Background(), *configPath); err != nil {
		slog.Error("mcp server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	database, err := db.NewDB(cfg.Database)
	if err != nil {
		return err
	}
	// GORM's default logger writes to stdout, which would corrupt MCP stdio.
	database = database.Session(&gorm.Session{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	sqlDB, err := database.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	var embedder retrieval.Embedder
	if cfg.LLM.APIKey != "" && cfg.LLM.EmbeddingModel != "" {
		client, err := llm.NewClient(llm.Config{
			BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey,
			Model: cfg.LLM.Model, EmbeddingModel: cfg.LLM.EmbeddingModel,
			Timeout: time.Duration(cfg.LLM.TimeoutSeconds) * time.Second,
		})
		if err != nil {
			return err
		}
		embedder = client
	}
	reader := memory.NewMemoryManger(database, retrieval.New(database, embedder))
	sessions := session.NewSessionManager(database)
	return appmcp.NewMcpMemoryReaderServer(reader, sessions).Run(ctx, &mcp.StdioTransport{})
}
