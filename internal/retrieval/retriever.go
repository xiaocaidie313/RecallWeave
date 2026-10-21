package retrieval

import (
	"context"
	"math"
	"sort"
	"strings"

	"recallweave/internal/store"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Hit 是一条向量近邻。Similarity 用的是入库后的向量，和 memories.embedding 同一长度。
type Hit struct {
	Memory     store.Memory
	Similarity float64
}

// Retriever 只做近邻：嵌入查询、按余弦距离取候选、算相似度。
// 活跃度加权和关键词合并留在 memory。
type Retriever struct {
	db       *gorm.DB
	embedder Embedder
}

func New(db *gorm.DB, embedder Embedder) *Retriever {
	return &Retriever{db: db, embedder: embedder}
}

// Search 返回按余弦相似度从高到低排列的候选。
// limit 是数据库取出的条数。调用方若还要按活跃度重排，应传入比最终条数更大的值。
// 没配嵌入模型、查询为空、或向量长度对不上时返回空结果，不视为错误。
func (r *Retriever) Search(ctx context.Context, query string, tag store.MemoryTag, limit int) ([]Hit, error) {
	if r == nil || r.embedder == nil || r.db == nil || strings.TrimSpace(query) == "" || limit <= 0 {
		return nil, nil
	}

	raw, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	// 和写入记忆走同一个 NewEmbedding，比较的才是截断并归一化后的 1536 维。
	queryEmbedding := store.NewEmbedding(raw)
	queryValues := store.EmbeddingValues(queryEmbedding)
	if len(queryValues) == 0 {
		return nil, nil
	}

	dbQuery := r.db.WithContext(ctx).Model(&store.Memory{}).Where("embedding IS NOT NULL")
	if tag != "" {
		dbQuery = dbQuery.Where("tag = ?", tag)
	}

	var memories []store.Memory
	err = dbQuery.Clauses(clause.OrderBy{
		Expression: clause.Expr{SQL: "embedding <=> ?", Vars: []any{queryEmbedding}},
	}).Limit(limit).Find(&memories).Error
	if err != nil {
		return nil, err
	}

	hits := make([]Hit, 0, len(memories))
	for _, item := range memories {
		similarity := CosineSimilarity(queryValues, store.EmbeddingValues(item.Embedding))
		if similarity <= 0 {
			continue
		}
		hits = append(hits, Hit{Memory: item, Similarity: similarity})
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Similarity > hits[j].Similarity
	})
	return hits, nil
}

// CosineSimilarity 计算余弦相似度。长度不一致或任一向量为零时返回 0。
func CosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
