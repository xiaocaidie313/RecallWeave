package useage

import "time"

type TokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64
	CachedTokens     int64
}

type ModelCallStats struct {
	RunID   string
	Model   string
	Round   int
	Usage   TokenUsage
	Cost    float64
	Latency time.Duration
}
