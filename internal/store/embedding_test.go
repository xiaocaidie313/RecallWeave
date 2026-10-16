package store

import "testing"

func TestNewEmbedding(t *testing.T) {
	if got := EmbeddingValues(NewEmbedding(nil)); len(got) != 0 {
		t.Fatalf("empty input should stay empty, got %d values", len(got))
	}

	short := make([]float64, EmbeddingDim-1)
	if got := EmbeddingValues(NewEmbedding(short)); len(got) != 0 {
		t.Fatalf("wrong dimension should stay empty, got %d values", len(got))
	}

	values := make([]float64, EmbeddingDim)
	values[0] = 1.5
	got := EmbeddingValues(NewEmbedding(values))
	if len(got) != EmbeddingDim || got[0] != 1.5 {
		t.Fatalf("expected dimension %d and first value 1.5, got len %d first %v", EmbeddingDim, len(got), got[0])
	}
}
