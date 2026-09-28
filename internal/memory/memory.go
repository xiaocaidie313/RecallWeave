package memory

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strings"

	"recallweave/internal/llm"
	"recallweave/internal/store"

	"gorm.io/gorm"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20
)

type MemoryManger struct {
	embedder llm.Embedder
	db       *gorm.DB
}

// 预加载历史对话的 总计的清单 让llm 不重新 根据全部历史记录 重新生成 而是 直接从清单里 提取 记忆
type MemoryBriefParam struct {
	ConversationID uint   `json:"conversation_id"`
	SessionID      []uint `json:"session_id"`
	// Brief string `json:"brief"`
	// raw ? 如果太多 ?
}

func NewMemoryManger(db *gorm.DB, embedder llm.Embedder) *MemoryManger {
	return &MemoryManger{db: db, embedder: embedder}
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

func (m *MemoryManger) SearchMemories(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	keywordHits, err := m.keywordSearch(ctx, keyword, tag, limit)
	if err != nil {
		return nil, err
	}

	vectorHits, err := m.vectorSearch(ctx, keyword, tag, limit)
	if err != nil {
		slog.Warn("vector search failed", "error", err)
		return keywordHits, nil
	}
	return mergeMemories(vectorHits, keywordHits, limit), nil
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

// vectorSearch 把检索词变成向量，和每条已有向量的记忆比相似度，取得分最高的几条。
func (m *MemoryManger) vectorSearch(ctx context.Context, keyword string, tag store.MemoryTag, limit int) ([]store.Memory, error) {
	if m.embedder == nil || strings.TrimSpace(keyword) == "" {
		return nil, nil
	}

	queryVector, err := m.embedder.Embed(ctx, keyword)
	if err != nil {
		return nil, err
	}

	query := m.db.WithContext(ctx).Model(&store.Memory{})
	if tag != "" {
		query = query.Where("tag = ?", tag)
	}

	// 很直白啊  搜索所有的记忆  这样肯定是不行的
	var memories []store.Memory
	if err := query.Find(&memories).Error; err != nil {
		return nil, err
	}

	type scoredMemory struct {
		memory store.Memory
		score  float64
	}
	scored := make([]scoredMemory, 0, len(memories))
	for _, item := range memories {
		if len(item.Embedding) == 0 {
			continue
		}
		score := CalculateSimilarity(queryVector, item.Embedding)
		if score <= 0 {
			continue
		}
		scored = append(scored, scoredMemory{memory: item, score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}

	hits := make([]store.Memory, 0, len(scored))
	for _, item := range scored {
		hits = append(hits, item.memory)
	}
	return hits, nil
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

	session = store.Session{
		ConversationID: &conversationID,
		SourceTag:      store.SourceChat,
		Name:           "chat",
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

// 根据conversationID 找 session 找 memmory 然后汇总
func (m *MemoryManger) GetMemoryBrief(ctx context.Context, conversationID uint) (MemoryBriefParam, error) {
	var relatedSessions []store.Session
	if err := m.db.WithContext(ctx).Model(&store.Session{}).Where("conversation_id = ?", conversationID).Find(&relatedSessions).Error; err != nil {
		return MemoryBriefParam{}, err
	}
	var sessionIDs []uint
	for _, session := range relatedSessions {
		sessionIDs = append(sessionIDs, session.ID)
	}
	return MemoryBriefParam{
		ConversationID: conversationID,
		SessionID:      sessionIDs,
	}, nil
}

func CalculateSimilarity(a, b []float64) float64 {
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
