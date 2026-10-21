package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/ask"
	"recallweave/internal/config"
	"recallweave/internal/db"
	"recallweave/internal/extract"
	apphttp "recallweave/internal/http"
	"recallweave/internal/ingest"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/retrieval"
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

	// 没配 api_key 时提炼退回整段兜底，问答直接不可用
	var client *llm.Client
	if cfg.LLM.APIKey == "" {
		logger.Info("llm api key is empty, extraction falls back to whole session and ask is disabled")
	} else {
		client, err = llm.NewClient(llm.Config{
			BaseURL:        cfg.LLM.BaseURL,
			APIKey:         cfg.LLM.APIKey,
			Model:          cfg.LLM.Model,
			EmbeddingModel: cfg.LLM.EmbeddingModel,
			Timeout:        time.Duration(cfg.LLM.TimeoutSeconds) * time.Second,
		})
		if err != nil {
			logger.Error("failed to create llm client", "error", err)
			os.Exit(1)
		}
	}

	// client 为 nil 时不能直接赋给接口变量，否则接口不为 nil，下游判断会失效
	var segmenter llm.Segmenter
	var embedder llm.Embedder
	var vectorEmbedder retrieval.Embedder
	var ChatGenerator llm.ChatGenerator
	if client != nil {
		segmenter = client
		embedder = client
		vectorEmbedder = client
		ChatGenerator = client
	}

	// 组装依赖。HTTP 在 internal/http，这里只把业务对象传进去。
	extracter := extract.NewExtractExcuter(database, segmenter, embedder)
	memoryManger := memory.NewMemoryManger(database, retrieval.New(database, vectorEmbedder))
	recall := agent.NewAgent(1, "recall", ChatGenerator, "RecallWeave 的助手", 6,
		ability.NewMemoryAbility(memoryManger),
	)
	askService := ask.NewAgent(recall, memoryManger, extracter)

	r := apphttp.NewRouter(
		apphttp.NewIngestHandler(ingest.NewService(database, extracter)),
		apphttp.NewExtractHandler(extracter),
		apphttp.NewAskHandler(askService),
		apphttp.NewConversationHandler(memoryManger),
	)

	address := fmt.Sprintf(":%d", cfg.Server.Port)
	logger.Info("RecallWeave API started", "address", address)

	if err := r.Run(address); err != nil {
		logger.Error("failed to start server", "error", err)
		os.Exit(1)
	}
}
