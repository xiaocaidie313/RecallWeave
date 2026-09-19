package main

import (
	"fmt"
	"log/slog"
	"os"
	"recallweave/internal/config"
	apphttp "recallweave/internal/http"
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

	// 启动路由
	r := apphttp.NewRouter()

	address := fmt.Sprintf(":%d", cfg.Server.Port)
	logger.Info("RecallWeave API started", "address", address)

	if err := r.Run(address); err != nil {
		logger.Error("failed to start server", "error", err)
		os.Exit(1)
	}
}
