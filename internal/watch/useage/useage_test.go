package useage

import (
	"testing"

	"recallweave/internal/llm"
)

func TestFromLLMAndSum(t *testing.T) {
	got := FromLLM(llm.Usage{PromptTokens: 4, CompletionTokens: 1, ReasoningTokens: 2, CachedTokens: 3})
	if got.PromptTokens != 4 || got.CompletionTokens != 1 || got.ReasoningTokens != 2 || got.CachedTokens != 3 {
		t.Fatalf("unexpected usage: %+v", got)
	}

	total := Sum([]ModelCallStats{
		{RunID: "r1", Model: "m", Usage: TokenUsage{PromptTokens: 10, CompletionTokens: 2}, LatencyMs: 12},
		{RunID: "r1", Model: "m", Usage: TokenUsage{PromptTokens: 5, CompletionTokens: 7, CachedTokens: 1}, LatencyMs: 8},
	})
	if total.RunID != "r1" || total.Model != "m" {
		t.Fatalf("unexpected identity: %+v", total)
	}
	if total.Usage.PromptTokens != 15 || total.Usage.CompletionTokens != 9 || total.Usage.CachedTokens != 1 {
		t.Fatalf("unexpected totals: %+v", total.Usage)
	}
	if total.LatencyMs != 20 {
		t.Fatalf("expected 20ms, got %d", total.LatencyMs)
	}
}
