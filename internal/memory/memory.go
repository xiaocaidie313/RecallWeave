package memory

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"

	"recallweave/internal/retrieval"
	"recallweave/internal/types"
	"time"

	"gorm.io/gorm"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20
	// recallCandidateFactor 为两路分别保留候选，融合后才截取最终 Top-K。
	// 向量路仍只查询这一批，不重复放大候选数量。
	recallCandidateFactor = 5
	rrfRankConstant       = 60

	// recallHalfLifeDays 是活跃度的半衰期。超过这个时长没被命中的记忆，权重按时间折半衰减。
	recallHalfLifeDays = 7
)

type MemoryManger struct {
	db        *gorm.DB
	retriever *retrieval.Retriever
}

func NewMemoryManger(db *gorm.DB, retriever *retrieval.Retriever) *MemoryManger {
	return &MemoryManger{db: db, retriever: retriever}
}

// FindMemories 混合检索，不改活跃度。预取背景用这个，避免每次提问都给 Top-K 加分。
func (m *MemoryManger) FindMemories(ctx context.Context, keyword string, tag types.MemoryTag, limit int, includeInactive ...bool) ([]types.Memory, error) {
	history := len(includeInactive) > 0 && includeInactive[0]
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	// 候选窗口
	candidateLimit := limit * recallCandidateFactor
	// 两路各自召回候选，不能提前裁到最终 Top-K。
	keywordHits, err := m.keywordSearch(ctx, keyword, tag, candidateLimit, history)
	if err != nil {
		return nil, err
	}
	// 向量检索
	vectorHits, err := m.vectorSearch(ctx, keyword, tag, candidateLimit, history)
	if err != nil {
		slog.Warn("vector search failed", "error", err)
		vectorHits = nil
	}
	return mergeMemories(vectorHits, keywordHits, limit), nil
}

// SearchMemories 给模型工具用：检索后再记一次召回。
func (m *MemoryManger) SearchMemories(ctx context.Context, keyword string, tag types.MemoryTag, limit int, includeInactive ...bool) ([]types.Memory, error) {
	merged, err := m.FindMemories(ctx, keyword, tag, limit, includeInactive...)
	if err != nil {
		return nil, err
	}
	if len(merged) > 0 {
		go m.bumpRecall(context.WithoutCancel(ctx), merged)
	}
	return merged, nil
}

func (m *MemoryManger) keywordSearch(ctx context.Context, keyword string, tag types.MemoryTag, limit int, includeInactive bool) ([]types.Memory, error) {
	query := m.db.WithContext(ctx).Model(&types.Memory{})
	if !includeInactive {
		query = query.Where("status = ?", types.MemoryActive)
	}
	if tokens := strings.Fields(keyword); len(tokens) > 0 {
		conditions := m.db.Session(&gorm.Session{NewDB: true})
		for _, token := range tokens {
			like := "%" + token + "%"
			conditions = conditions.Or("title LIKE ? OR content LIKE ?", like, like)
		}
		query = query.Where(conditions)
	}
	if tag != "" {
		query = query.Where("tag = ?", tag)
	}

	var memories []types.Memory
	if err := query.Order("id DESC").Limit(limit).Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

// vectorSearch 取一批向量近邻，再按活跃度重排。近邻本身在 retrieval。
func (m *MemoryManger) vectorSearch(ctx context.Context, keyword string, tag types.MemoryTag, limit int, includeInactive bool) ([]types.Memory, error) {
	if m.retriever == nil {
		return nil, nil
	}

	// limit 已是候选窗口；调用方会在两路融合后裁到最终数量。
	hits, err := m.retriever.Search(ctx, keyword, tag, limit, includeInactive)
	if err != nil {
		return nil, err
	}

	type scoredMemory struct {
		memory types.Memory
		score  float64
	}
	scored := make([]scoredMemory, 0, len(hits))
	now := time.Now()
	for _, hit := range hits {
		// 相似度 × 活跃度：向量命中后，越常被用、越最近用的记忆排序越靠前。
		score := hit.Similarity * activationMultiplier(hit.Memory.RecalledCount, hit.Memory.LastAccessAt, now)
		scored = append(scored, scoredMemory{memory: hit.Memory, score: score})
	}
	// 按照score得分 排记忆
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].memory.ID > scored[j].memory.ID
		}
		return scored[i].score > scored[j].score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}

	ranked := make([]types.Memory, 0, len(scored))
	for _, item := range scored {
		ranked = append(ranked, item.memory)
	}
	return ranked, nil
}

// mergeMemories uses equal-weight reciprocal rank fusion: sum(1/(60+rank)).
// An item contributes at most once per route; rank starts at 1. This combines
// positions rather than incomparable keyword/vector scores. Zero rank means
// that the route did not return the item.
func mergeMemories(vector, keyword []types.Memory, limit int) []types.Memory {
	if limit <= 0 {
		return nil
	}
	type candidate struct {
		memory types.Memory
		ranks  [2]int
		score  float64
		best   int
	}
	byID := make(map[uint]*candidate, len(vector)+len(keyword))
	for route, group := range [][]types.Memory{vector, keyword} {
		for i, item := range group {
			entry := byID[item.ID]
			if entry == nil {
				entry = &candidate{memory: item, best: i + 1}
				byID[item.ID] = entry
			}
			if entry.ranks[route] != 0 {
				continue
			}
			rank := i + 1
			entry.ranks[route] = rank
			entry.score += 1 / float64(rrfRankConstant+rank)
			if rank < entry.best {
				entry.best = rank
			}
		}
	}
	ranked := make([]*candidate, 0, len(byID))
	for _, entry := range byID {
		ranked = append(ranked, entry)
	}
	sort.Slice(ranked, func(i, j int) bool {
		// 优先用分数排
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		// 分数相同  再按照历史最佳排
		if ranked[i].best != ranked[j].best {
			return ranked[i].best < ranked[j].best
		}
		// 都相同按照
		// 按照ID排
		return ranked[i].memory.ID > ranked[j].memory.ID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	merged := make([]types.Memory, 0, len(ranked))
	for _, entry := range ranked {
		merged = append(merged, entry.memory)
		slog.Debug("memory recall fusion", "memory_id", entry.memory.ID,
			"vector_rank", entry.ranks[0], "keyword_rank", entry.ranks[1], "rrf_score", entry.score)
	}
	return merged
}

// activationMultiplier 把记忆的「活跃度」折成一个乘数，叠到相似度上做排序。
// 召回次数越多乘数越大（log1p 压缩避免单条爆炸），但超过半衰期没被命中的记忆
// 按年龄指数衰减——常被用的活记忆浮上来，冷门老记忆沉底。全是纯计算，方便测试。
func activationMultiplier(recalled int, lastAccess time.Time, now time.Time) float64 {
	recallBoost := math.Log1p(float64(recalled)) * 0.5

	var decay float64
	if !lastAccess.IsZero() && now.After(lastAccess) {
		ageDays := now.Sub(lastAccess).Hours() / 24
		decay = math.Pow(0.5, ageDays/recallHalfLifeDays)
	} else {
		// 从没被命中的记忆保持中性权重，不额外衰减。
		decay = 1
	}

	return 1 + recallBoost*decay
}

// bumpRecall 给命中的记忆记一次召回：次数 +1、刷新最近命中时间。
// 在事务里批量更新，更新失败只记日志，不影响这次检索的结果。
// 把命中的memory 整体搜索到集体处理 +1
func (m *MemoryManger) bumpRecall(ctx context.Context, hits []types.Memory) {
	now := time.Now()
	// 直接拼 SQL 比逐条 Save 省一次查询，命中一多就不想逐条开事务。
	ids := make([]uint, 0, len(hits))
	for _, item := range hits {
		ids = append(ids, item.ID)
	}

	if err := m.db.WithContext(ctx).Model(&types.Memory{}).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"recalled_count": gorm.Expr("recalled_count + 1"),
			"last_access_at": now,
		}).Error; err != nil {
		slog.Warn("bump recall failed", "count", len(ids), "error", err)
	}
}

// ListBySession 返回一个会话下的记忆，按它在原文里的起始位置排序。
func (m *MemoryManger) ListBySession(ctx context.Context, sessionID uint, includeInactive ...bool) ([]types.Memory, error) {
	var memories []types.Memory
	query := m.db.WithContext(ctx).Where("session_id = ?", sessionID)
	if len(includeInactive) == 0 || !includeInactive[0] {
		query = query.Where("status = ?", types.MemoryActive)
	}
	if err := query.
		Order("seq_start").
		Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}
