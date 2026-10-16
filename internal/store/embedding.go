package store

import "github.com/pgvector/pgvector-go"

// EmbeddingDim 是 Qwen3-VL-Embedding-8B 的默认向量长度。
// pgvector 的列宽必须和这个长度一致。
const EmbeddingDim = 4096

// NewEmbedding 把嵌入接口返回的 float64 收成 pgvector 能写入的向量。
// 长度不对或为空时返回空向量，数据库里记成 NULL，这条记忆仍可凭关键词检索。
func NewEmbedding(values []float64) pgvector.Vector {
	if len(values) != EmbeddingDim {
		return pgvector.Vector{}
	}
	raw := make([]float32, len(values))
	for i, value := range values {
		raw[i] = float32(value)
	}
	return pgvector.NewVector(raw)
}

// float32 -> float64
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
