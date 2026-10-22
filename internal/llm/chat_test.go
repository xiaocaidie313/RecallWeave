package llm

import (
	"testing"

	openai "github.com/openai/openai-go/v3"
)

func TestUsageFromCompletion(t *testing.T) {
	got := usageFromCompletion(openai.CompletionUsage{
		PromptTokens:     21,
		CompletionTokens: 9,
		CompletionTokensDetails: openai.CompletionUsageCompletionTokensDetails{
			ReasoningTokens: 4,
		},
		PromptTokensDetails: openai.CompletionUsagePromptTokensDetails{
			CachedTokens: 6,
		},
	})
	if got.PromptTokens != 21 || got.CompletionTokens != 9 {
		t.Fatalf("unexpected tokens: %+v", got)
	}
	if got.ReasoningTokens != 4 || got.CachedTokens != 6 {
		t.Fatalf("unexpected details: %+v", got)
	}
}
