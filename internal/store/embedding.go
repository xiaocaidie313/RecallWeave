package store

import (
	"math"

	"github.com/pgvector/pgvector-go"
)

const (
	// ModelEmbeddingDim 是 Qwen3-VL-Embedding-8B 返回的完整长度。
	ModelEmbeddingDim = 4096
	// EmbeddingDim 是入库长度。HNSW 索引最多 2000 维，模型允许截取前 1536 维。
	EmbeddingDim = 1536
)

// NewEmbedding 把嵌入结果收成可写入 pgvector 的向量。
// 完整的 4096 维会截成前 1536 维并重新归一化。长度对不上时返回空向量，记成 NULL。
func NewEmbedding(values []float64) pgvector.Vector {
	switch len(values) {
	case EmbeddingDim:
		return pgvector.NewVector(toFloat32(values))
	case ModelEmbeddingDim:
		return pgvector.NewVector(toFloat32(normalize(values[:EmbeddingDim])))
	default:
		return pgvector.Vector{}
	}
}

// EmbeddingValues 把库存的向量还原成相似度计算用的 float64。
func EmbeddingValues(vector pgvector.Vector) []float64 {
	raw := vector.Slice()
	if len(raw) == 0 {
		return nil
	}
	values := make([]float64, len(raw))
	for i, value := range raw {
		values[i] = float64(value)
	}
	return values
}

func toFloat32(values []float64) []float32 {
	raw := make([]float32, len(values))
	for i, value := range values {
		raw[i] = float32(value)
	}
	return raw
}

func normalize(values []float64) []float64 {
	var sum float64
	for _, value := range values {
		sum += value * value
	}
	if sum == 0 {
		return values
	}
	scale := 1 / math.Sqrt(sum)
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = value * scale
	}
	return out
}
