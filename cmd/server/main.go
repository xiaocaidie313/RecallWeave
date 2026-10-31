package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/subagent"
	"recallweave/internal/artifact"
	"recallweave/internal/chat"
	"recallweave/internal/config"
	"recallweave/internal/db"
	"recallweave/internal/extract"
	apphttp "recallweave/internal/http"
	"recallweave/internal/ingest"
	"recallweave/internal/llm"
	appmcp "recallweave/internal/mcp"
	"recallweave/internal/memory"
	"recallweave/internal/retrieval"
	"recallweave/internal/skill"
)

const defaultConfigPath = "config/config.yaml"

func main() {
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// 加载配置
	cfg, err := config.LoadConfig(defaultConfigPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	delegator, err := newSubagentManager(cfg.Subagents)
	if err != nil {
		logger.Error("failed to configure subagents", "error", err)
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
	delegator.WithSubagentSessions(subagent.NewSubagentSessionStore(database))

	// 没配 api_key 时提炼退回整段兜底，问答直接不可用
	var client *llm.Client
	if cfg.LLM.APIKey == "" {
		logger.Info("llm api key is empty, extraction falls back to whole session and chat is disabled")
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
	skillDir := cfg.SkillDir()
	catalog, err := skill.Open(skillDir)
	if err != nil {
		logger.Error("failed to load skills", "error", err)
		os.Exit(1)
	}
	logger.Info("skills catalog loaded", "dir", skillDir, "count", len(catalog.List()))
	artifacts, err := artifact.Open(cfg.ArtifactDir())
	if err != nil {
		logger.Error("failed to open artifact directory", "error", err)
		os.Exit(1)
	}
	defer artifacts.Close()
	recall := agent.NewAgent(1, "recall", ChatGenerator, "RecallWeave 的助手", 6,
		ability.NewMemoryAbility(memoryManger),
		ability.NewArtifactAbility(artifacts),
	).WithSkills(catalog)
	if cfg.ExternalMCP.URL != "" {
		if cfg.ExternalMCP.TokenEnv != "" && os.Getenv(cfg.ExternalMCP.TokenEnv) == "" {
			logger.Error("external MCP token environment variable is empty", "name", cfg.ExternalMCP.TokenEnv)
			os.Exit(1)
		}
		remoteClient, err := appmcp.ConnectHTTP(context.Background(), cfg.ExternalMCP.URL, os.Getenv(cfg.ExternalMCP.TokenEnv))
		if err != nil {
			logger.Error("failed to connect external MCP", "error", err)
			os.Exit(1)
		}
		defer remoteClient.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		remoteTools, err := remoteClient.AgentTools(ctx, cfg.ExternalMCP.Tools)
		cancel()
		if err != nil {
			logger.Error("failed to load external MCP tools", "error", err)
			os.Exit(1)
		}
		if err := recall.WithExternalTools(remoteTools); err != nil {
			logger.Error("failed to register external MCP tools", "error", err)
			os.Exit(1)
		}
		logger.Info("external MCP tools registered", "count", len(remoteTools))
	}
	serviceCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := delegator.StartBackground(serviceCtx, subagent.NewTaskStore(database), subagent.BackgroundConfig{Workers: 2, QueueCapacity: 8}); err != nil {
		logger.Error("failed to start background subagents", "error", err)
		exitCode = 1
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := delegator.CloseBackground(ctx); err != nil {
			logger.Error("background subagent shutdown incomplete", "error", err)
		}
	}()
	chatService := chat.NewAgent(recall, memoryManger, extracter).
		WithDelegator(delegator)

	r := apphttp.NewRouter(
		apphttp.NewIngestHandler(ingest.NewService(database, extracter)),
		apphttp.NewExtractHandler(extracter),
		apphttp.NewChatHandler(chatService),
		apphttp.NewConversationHandler(memoryManger),
	)
	apphttp.MountSubagentTasks(r, apphttp.NewSubagentTaskHandler(delegator))
	// if token := os.Getenv("RECALLWEAVE_MCP_TOKEN"); token != "" {
	// 	apphttp.MountMCP(r, appmcp.NewMcpServer(memoryManger), token)
	// 	logger.Info("MCP Streamable HTTP enabled", "path", "/mcp")
	// } else {
	// 	logger.Info("MCP Streamable HTTP disabled; set RECALLWEAVE_MCP_TOKEN to enable it")
	// }
	apphttp.MountMCP(r, appmcp.NewMcpServer(memoryManger), os.Getenv("RECALLWEAVE_MCP_TOKEN"))

	address := fmt.Sprintf(":%d", cfg.Server.Port)
	logger.Info("RecallWeave API started", "address", address)

	server := &http.Server{Addr: address, Handler: r, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx }}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			exitCode = 1
		}
		stop()
	case <-serviceCtx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP shutdown timed out", "error", err)
		_ = server.Close()
	}
}
