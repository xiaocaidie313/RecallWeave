package store

import (
	"math"
	"testing"
)

func TestNewEmbedding(t *testing.T) {
	if got := EmbeddingValues(NewEmbedding(nil)); len(got) != 0 {
		t.Fatalf("empty input should stay empty, got %d values", len(got))
	}

	short := make([]float64, 10)
	if got := EmbeddingValues(NewEmbedding(short)); len(got) != 0 {
		t.Fatalf("wrong dimension should stay empty, got %d values", len(got))
	}

	stored := make([]float64, EmbeddingDim)
	stored[0] = 1.5
	got := EmbeddingValues(NewEmbedding(stored))
	if len(got) != EmbeddingDim || got[0] != 1.5 {
		t.Fatalf("expected stored vector to pass through, got len %d first %v", len(got), got[0])
	}

	full := make([]float64, ModelEmbeddingDim)
	full[0] = 3
	got = EmbeddingValues(NewEmbedding(full))
	if len(got) != EmbeddingDim {
		t.Fatalf("expected truncation to %d, got %d", EmbeddingDim, len(got))
	}
	if math.Abs(got[0]-1) > 1e-5 {
		t.Fatalf("expected truncated vector to be normalized, got %v", got[0])
	}
}
