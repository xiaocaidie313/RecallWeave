package retrieval

import (
	"math"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	if got := CosineSimilarity(nil, []float64{1}); got != 0 {
		t.Fatalf("empty vector should score 0, got %v", got)
	}
	if got := CosineSimilarity([]float64{1, 0}, []float64{1}); got != 0 {
		t.Fatalf("mismatched length should score 0, got %v", got)
	}
	if got := CosineSimilarity([]float64{1, 0}, []float64{0, 1}); got != 0 {
		t.Fatalf("orthogonal vectors should score 0, got %v", got)
	}
	if got := CosineSimilarity([]float64{3, 0}, []float64{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("parallel vectors should score 1, got %v", got)
	}
}
