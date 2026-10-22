package useage

import (
	"log/slog"

	"recallweave/internal/llm"
)

type TokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
}

type ModelCallStats struct {
	RunID     string     `json:"run_id"`
	Model     string     `json:"model,omitempty"`
	Round     int        `json:"round"`
	Usage     TokenUsage `json:"usage"`
	LatencyMs int64      `json:"latency_ms"`
}

func FromLLM(usage llm.Usage) TokenUsage {
	return TokenUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CachedTokens:     usage.CachedTokens,
	}
}

func Record(stats ModelCallStats) {
	slog.Info("model call",
		"run_id", stats.RunID,
		"model", stats.Model,
		"round", stats.Round,
		"prompt_tokens", stats.Usage.PromptTokens,
		"completion_tokens", stats.Usage.CompletionTokens,
		"reasoning_tokens", stats.Usage.ReasoningTokens,
		"cached_tokens", stats.Usage.CachedTokens,
		"latency_ms", stats.LatencyMs,
	)
}

func Sum(calls []ModelCallStats) ModelCallStats {
	var total ModelCallStats
	for _, call := range calls {
		if total.RunID == "" {
			total.RunID = call.RunID
			total.Model = call.Model
		}
		total.Usage.PromptTokens += call.Usage.PromptTokens
		total.Usage.CompletionTokens += call.Usage.CompletionTokens
		total.Usage.ReasoningTokens += call.Usage.ReasoningTokens
		total.Usage.CachedTokens += call.Usage.CachedTokens
		total.LatencyMs += call.LatencyMs
	}
	return total
}
