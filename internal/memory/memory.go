package memory

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strings"

	"recallweave/internal/retrieval"
	"recallweave/internal/store"
	"time"

	"gorm.io/gorm"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20
	// vectorCandidateFactor 是数据库按距离先取出的候选倍数。
	// 活跃度加权在这批结果上重排，不再把整表读进内存。
	vectorCandidateFactor = 5

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

func (m *MemoryManger) ConversationExists(ctx context.Context, conversationID uint) (bool, error) {
	var count int64
	err := m.db.WithContext(ctx).Model(&store.Conversation{}).Where("id = ?", conversationID).Count(&count).Error
	return count > 0, err
}

func (m *MemoryManger) ConversationTitle(ctx context.Context, conversationID uint) (string, error) {
	var item store.Conversation
	err := m.db.WithContext(ctx).Select("title").First(&item, conversationID).Error
	if err != nil {
		return "", err
	}
	return item.Title, nil
}

func (m *MemoryManger) ConversationBrief(ctx context.Context, conversationID uint) (string, int, error) {
	var item store.Conversation
	err := m.db.WithContext(ctx).Select("brief", "brief_until_seq").First(&item, conversationID).Error
	if err != nil {
		return "", 0, err
	}
	return item.Brief, item.BriefUntilSeq, nil
}

func (m *MemoryManger) UpdateConversationBrief(ctx context.Context, conversationID uint, brief string, untilSeq int) error {
	return m.db.WithContext(ctx).Model(&store.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]any{
			"brief":           brief,
			"brief_until_seq": untilSeq,
		}).Error
}

func (m *MemoryManger) UpdateConversationTitle(ctx context.Context, conversationID uint, title string) error {
	return m.db.WithContext(ctx).Model(&store.Conversation{}).
		Where("id = ?", conversationID).
		Update("title", title).Error
}

func (m *MemoryManger) CreateConversation(ctx context.Context, title string) (store.Conversation, error) {
	item := store.Conversation{Title: title}
	err := m.db.WithContext(ctx).Create(&item).Error
	return item, err
}

func (m *MemoryManger) ListConversations(ctx context.Context) ([]store.Conversation, error) {
	var items []store.Conversation
	err := m.db.WithContext(ctx).Order("updated_at DESC, id DESC").Find(&items).Error
	return items, err
}

func (m *MemoryManger) ListConversationsWithTitle(ctx context.Context) ([]struct {
	ID        uint      `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
}, error) {
	var items = []struct {
		ID        uint      `json:"id"`
		Title     string    `json:"title"`
		UpdatedAt time.Time `json:"updated_at"`
	}{}

	err := m.db.WithContext(ctx).
		Model(&store.Conversation{}).
		Select("id", "title", "updated_at").
		Order("updated_at DESC, id DESC").
		Find(&items).Error
	if err != nil {
		return nil, err
	}
	return items, err
}

// FindMemories 混合检索，不改活跃度。预取背景用这个，避免每次提问都给 Top-K 加分。
func (m *MemoryManger) FindMemories(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	// 关键词检索
	keywordHits, err := m.keywordSearch(ctx, keyword, tag, limit)
	if err != nil {
		return nil, err
	}
	// 向量检索
	vectorHits, err := m.vectorSearch(ctx, keyword, tag, limit)
	if err != nil {
		slog.Warn("vector search failed", "error", err)
		vectorHits = nil
	}
	return mergeMemories(vectorHits, keywordHits, limit), nil
}

// SearchMemories 给模型工具用：检索后再记一次召回。
func (m *MemoryManger) SearchMemories(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	merged, err := m.FindMemories(ctx, keyword, tag, limit)
	if err != nil {
		return nil, err
	}
	if len(merged) > 0 {
		go m.bumpRecall(context.WithoutCancel(ctx), merged)
	}
	return merged, nil
}

func (m *MemoryManger) keywordSearch(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	query := m.db.WithContext(ctx).Model(&store.Memory{})
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

	var memories []store.Memory
	if err := query.Order("id DESC").Limit(limit).Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

// vectorSearch 取一批向量近邻，再按活跃度重排。近邻本身在 retrieval。
func (m *MemoryManger) vectorSearch(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	if m.retriever == nil {
		return nil, nil
	}

	// 多取几条再重排，避免活跃度加权只在最终的几条里打转。
	hits, err := m.retriever.Search(ctx, keyword, tag, limit*vectorCandidateFactor)
	if err != nil {
		return nil, err
	}

	type scoredMemory struct {
		memory store.Memory
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
		return scored[i].score > scored[j].score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}

	ranked := make([]store.Memory, 0, len(scored))
	for _, item := range scored {
		ranked = append(ranked, item.memory)
	}
	return ranked, nil
}

func mergeMemories(primary, extra []store.Memory, limit int) []store.Memory {
	merged := make([]store.Memory, 0, limit)
	seen := make(map[uint]struct{})
	for _, group := range [][]store.Memory{primary, extra} {
		for _, item := range group {
			if _, ok := seen[item.ID]; ok {
				continue
			}
			seen[item.ID] = struct{}{}
			merged = append(merged, item)
			if len(merged) == limit {
				return merged
			}
		}
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
func (m *MemoryManger) bumpRecall(ctx context.Context, hits []store.Memory) {
	now := time.Now()
	// 直接拼 SQL 比逐条 Save 省一次查询，命中一多就不想逐条开事务。
	ids := make([]uint, 0, len(hits))
	for _, item := range hits {
		ids = append(ids, item.ID)
	}

	if err := m.db.WithContext(ctx).Model(&store.Memory{}).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"recalled_count": gorm.Expr("recalled_count + 1"),
			"last_access_at": now,
		}).Error; err != nil {
		slog.Warn("bump recall failed", "count", len(ids), "error", err)
	}
}

// ListBySession 返回一个会话下的记忆，按它在原文里的起始位置排序。
func (m *MemoryManger) ListBySession(ctx context.Context, sessionID uint) ([]store.Memory, error) {
	var memories []store.Memory
	if err := m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("seq_start").
		Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

// GetMessages 按 seq 区间把某条记忆的原文捞回来，这是「可追溯」的落点。
func (m *MemoryManger) GetMessages(ctx context.Context, sessionID uint, seqStart, seqEnd int) ([]store.Message, error) {
	var messages []store.Message
	if err := m.db.WithContext(ctx).
		Where("session_id = ? AND seq >= ? AND seq <= ?", sessionID, seqStart, seqEnd).
		Order("seq").
		Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// ChatSession 是跟助手的这一次聊天。同一个 conversation 只建一条，
// 导入的其他来源会话不放进来。
func (m *MemoryManger) ChatSession(ctx context.Context, conversationID uint) (*store.Session, error) {
	exists, err := m.ConversationExists(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gorm.ErrRecordNotFound
	}

	var session store.Session
	err = m.db.WithContext(ctx).
		Where("conversation_id = ? AND source_tag = ?", conversationID, store.SourceChat).
		First(&session).Error
	if err == nil {
		return &session, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	// 没找到就新建
	session = store.Session{
		ConversationID: &conversationID,
		SourceTag:      store.SourceChat,
		Name:           "chat",
		ExtractStatus:  store.ExtractPending,
	}
	if err := m.db.WithContext(ctx).Create(&session).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

// ListMessages 按顺序返回一次聊天里已经保存的消息。
func (m *MemoryManger) ListMessages(ctx context.Context, sessionID uint) ([]store.Message, error) {
	var messages []store.Message
	err := m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("seq").
		Find(&messages).Error
	return messages, err
}

// AppendMessages 把新消息接在已有序号后面。
func (m *MemoryManger) AppendMessages(ctx context.Context, sessionID uint, messages []store.Message) error {
	if len(messages) == 0 {
		return nil
	}

	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var last store.Message
		err := tx.Where("session_id = ?", sessionID).Order("seq desc").First(&last).Error
		seq := 0
		if err == nil {
			seq = last.Seq
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		for i := range messages {
			seq++
			messages[i].SessionID = sessionID
			messages[i].Seq = seq
		}
		return tx.Create(&messages).Error
	})
}

func (m *MemoryManger) GetSessions(ctx context.Context, conversationID uint) ([]store.Session, error) {
	var sessions []store.Session
	if err := m.db.WithContext(ctx).Model(&store.Session{}).Where("conversation_id = ?", conversationID).Find(&sessions).Error; err != nil {
		return nil, err
	}
	return sessions, nil
}

type SessionOverview struct {
	Session      store.Session
	MessageCount int64
	MemoryCount  int64
}

func (m *MemoryManger) GetSession(ctx context.Context, sessionID uint) (store.Session, error) {
	var session store.Session
	err := m.db.WithContext(ctx).First(&session, sessionID).Error
	return session, err
}

func (m *MemoryManger) SessionOverview(ctx context.Context, sessionID uint) (SessionOverview, error) {
	session, err := m.GetSession(ctx, sessionID)
	if err != nil {
		return SessionOverview{}, err
	}

	var messages, memories int64
	if err := m.db.WithContext(ctx).Model(&store.Message{}).Where("session_id = ?", sessionID).Count(&messages).Error; err != nil {
		return SessionOverview{}, err
	}
	if err := m.db.WithContext(ctx).Model(&store.Memory{}).Where("session_id = ?", sessionID).Count(&memories).Error; err != nil {
		return SessionOverview{}, err
	}
	return SessionOverview{
		Session:      session,
		MessageCount: messages,
		MemoryCount:  memories,
	}, nil
}

func (m *MemoryManger) SaveAgentRun(ctx context.Context, run store.AgentRun) error {
	if m == nil || m.db == nil || run.RunID == "" {
		return nil
	}
	return m.db.WithContext(ctx).Create(&run).Error
}

func (m *MemoryManger) GetAgentRun(ctx context.Context, runID string) (store.AgentRun, error) {
	var run store.AgentRun
	err := m.db.WithContext(ctx).Where("run_id = ?", runID).First(&run).Error
	return run, err
}

func (m *MemoryManger) ListAgentRuns(ctx context.Context, conversationID uint) ([]store.AgentRun, error) {
	var runs []store.AgentRun
	err := m.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id DESC").
		Limit(20).
		Find(&runs).Error
	return runs, err
}

func (m *MemoryManger) FindChatSession(ctx context.Context, conversationID uint) (*store.Session, error) {
	var session store.Session
	err := m.db.WithContext(ctx).
		Where("conversation_id = ? AND source_tag = ?", conversationID, store.SourceChat).
		First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}
