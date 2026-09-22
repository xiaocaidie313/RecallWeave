package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"recallweave/internal/config"
	"recallweave/internal/db"
	"recallweave/internal/extract"
	apphttp "recallweave/internal/http"
	"recallweave/internal/ingest"
	"recallweave/internal/llm"
)

const defaultConfigPath = "config/config.yaml"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// 加载配置
	cfg, err := config.LoadConfig(defaultConfigPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// 连接数据库并建表
	database, err := db.NewDB(cfg.Database)
	if err != nil {
		logger.Error("failed to connect database", "error", err)
		os.Exit(1)
	}

	if err := db.AutoMigrate(database); err != nil {
		logger.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}

	// 没配 api_key 时 segmenter 为 nil，提炼退回整段兜底
	var segmenter llm.Segmenter
	if cfg.LLM.APIKey == "" {
		logger.Info("llm api key is empty, extraction falls back to whole session")
	} else {
		client, err := llm.NewClient(llm.Config{
			BaseURL: cfg.LLM.BaseURL,
			APIKey:  cfg.LLM.APIKey,
			Model:   cfg.LLM.Model,
			Timeout: time.Duration(cfg.LLM.TimeoutSeconds) * time.Second,
		})
		if err != nil {
			logger.Error("failed to create llm client", "error", err)
			os.Exit(1)
		}
		segmenter = client
	}

	// 组装依赖
	ingestHandler := ingest.NewIngestHandler(ingest.NewIngestService(ingest.NewIngestRepo(database)))
	extractHandler := extract.NewExtractHandler(extract.NewExtractExcuter(database, segmenter))

	// 启动路由
	r := apphttp.NewRouter(ingestHandler, extractHandler)

	address := fmt.Sprintf(":%d", cfg.Server.Port)
	logger.Info("RecallWeave API started", "address", address)

	if err := r.Run(address); err != nil {
		logger.Error("failed to start server", "error", err)
		os.Exit(1)
	}
}
