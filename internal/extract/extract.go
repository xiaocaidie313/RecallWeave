package extract

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"recallweave/internal/llm"
	"recallweave/internal/store"

	"gorm.io/gorm"
)

// Title 字段是 varchar(191)，这里留出余量，按字符数而不是字节数截断。
const maxTitleLength = 60

const (
	segmentBatchMaxMessages = 20
	segmentBatchMaxChars    = 8000
	segmentBatchOverlap     = 2
)

type ExtractExcuter struct {
	db        *gorm.DB
	segmenter llm.Segmenter
	embedder  llm.Embedder
}

// segmenter 传 nil 时退回本地兜底：每一批消息合成一段，标题取该批第一条的第一行。
func NewExtractExcuter(db *gorm.DB, segmenter llm.Segmenter, embedder llm.Embedder) *ExtractExcuter {
	return &ExtractExcuter{db: db, segmenter: segmenter, embedder: embedder}
}

// ExtractSession 把一个会话提炼成若干条记忆，返回生成的条数。
// 重复提炼同一个会话是幂等的：先删掉旧记忆，再写新的。
func (e *ExtractExcuter) ExtractSessionToMemory(ctx context.Context, sessionID uint) (int, error) {
	count, err := e.extractSessionToMemory(ctx, sessionID)
	e.markExtract(ctx, sessionID, err)
	return count, err
}

func (e *ExtractExcuter) extractSessionToMemory(ctx context.Context, sessionID uint) (int, error) {
	messages, err := e.GetMessages(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	if len(messages) == 0 {
		return 0, nil
	}

	memories := e.memoriesFrom(ctx, sessionID, messages)
	if err := e.replaceMemories(ctx, sessionID, memories); err != nil {
		return 0, err
	}

	return len(memories), nil
}

// ExtractNew 只提炼上次覆盖序号之后的新消息，已有记忆保留。
// 手动的 ExtractSession 仍会删掉旧记忆再整段重写。
// 摘要失败做降级 ----> llm 自动重新提炼信息
func (e *ExtractExcuter) ExtractNewToMemory(ctx context.Context, sessionID uint) (int, error) {
	count, err := e.extractNewToMemory(ctx, sessionID)
	e.markExtract(ctx, sessionID, err)
	return count, err
}

func (e *ExtractExcuter) extractNewToMemory(ctx context.Context, sessionID uint) (int, error) {
	messages, err := e.GetMessages(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(messages) == 0 {
		return 0, nil
	}

	covered, err := e.coveredSeq(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	fresh := make([]store.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Seq > covered {
			fresh = append(fresh, msg)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}

	memories := e.memoriesFrom(ctx, sessionID, fresh)
	if err := e.db.WithContext(ctx).Create(&memories).Error; err != nil {
		return 0, err
	}
	return len(memories), nil
}

func (e *ExtractExcuter) markExtract(ctx context.Context, sessionID uint, extractErr error) {
	if e.db == nil {
		return
	}
	updates := map[string]any{
		"extract_error": "",
	}
	if extractErr != nil {
		updates["extract_status"] = store.ExtractFailed
		updates["extract_error"] = extractErr.Error()
	} else {
		now := time.Now()
		updates["extract_status"] = store.ExtractDone
		updates["extracted_at"] = now
	}
	if err := e.db.WithContext(ctx).Model(&store.Session{}).Where("id = ?", sessionID).Updates(updates).Error; err != nil {
		slog.Warn("mark extract status failed", "session_id", sessionID, "error", err)
	}
}

func (e *ExtractExcuter) coveredSeq(ctx context.Context, sessionID uint) (int, error) {
	var row struct {
		Seq int
	}
	err := e.db.WithContext(ctx).Model(&store.Memory{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq_end), 0) AS seq").
		Scan(&row).Error
	return row.Seq, err
}

func (e *ExtractExcuter) memoriesFrom(ctx context.Context, sessionID uint, messages []store.Message) []store.Memory {
	segments := e.segment(ctx, messages)
	memories := make([]store.Memory, 0, len(segments))
	for _, segment := range segments {
		memories = append(memories, store.Memory{
			SessionID: sessionID,
			SeqStart:  segment.SeqStart,
			SeqEnd:    segment.SeqEnd,
			Title:     truncateTitle(segment.Title),
			Content:   segment.Summary,
			Tag:       store.NormalizeMemoryTag(segment.Tag),
			Embedding: store.NewEmbedding(e.embedMemory(ctx, segment.Title, segment.Summary)),
		})
	}
	return memories
}

// embedMemory 给一条记忆算向量。没配嵌入模型或调用失败时返回空，记忆仍会写入。
func (e *ExtractExcuter) embedMemory(ctx context.Context, title, content string) []float64 {
	if e.embedder == nil {
		return nil
	}

	vector, err := e.embedder.Embed(ctx, title+"\n"+content)
	if err != nil {
		slog.Warn("embed memory failed", "error", err)
		return nil
	}
	if len(vector) != store.ModelEmbeddingDim && len(vector) != store.EmbeddingDim {
		slog.Warn("unexpected embedding dimension", "got", len(vector), "want", store.ModelEmbeddingDim)
		return nil
	}
	return vector
}

// #1 读取消息
func (e *ExtractExcuter) GetMessages(ctx context.Context, sessionID uint) ([]store.Message, error) {
	var messages []store.Message

	if err := e.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("seq").Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// #2 切段。长会话按批交给模型，单批失败只降级该批。
func (e *ExtractExcuter) segment(ctx context.Context, messages []store.Message) []llm.Segment {
	if len(messages) == 0 {
		return nil
	}

	var segments []llm.Segment
	for _, batch := range splitMessageBatches(messages) {
		segments = append(segments, e.segmentBatch(ctx, batch)...)
	}
	return mergeBatchSegments(segments)
}

func (e *ExtractExcuter) segmentBatch(ctx context.Context, messages []store.Message) []llm.Segment {
	fallback := []llm.Segment{wholeSessionSegment(messages)}
	if e.segmenter == nil {
		return fallback
	}

	input := make([]llm.Message, 0, len(messages))
	for _, msg := range messages {
		input = append(input, llm.Message{
			Seq:     msg.Seq,
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	segments, err := e.segmenter.Segment(ctx, input, allowedTags())
	if err != nil {
		slog.Warn("segment batch failed, falling back to this batch",
			"session_id", messages[0].SessionID,
			"seq_start", messages[0].Seq,
			"seq_end", messages[len(messages)-1].Seq,
			"error", err,
		)
		return fallback
	}

	valid := keepValidSegments(segments, messages)
	if len(valid) == 0 {
		slog.Warn("model returned no usable segment, falling back to this batch",
			"session_id", messages[0].SessionID,
			"seq_start", messages[0].Seq,
			"seq_end", messages[len(messages)-1].Seq,
		)
		return fallback
	}
	return valid
}

func splitMessageBatches(messages []store.Message) [][]store.Message {
	if len(messages) == 0 {
		return nil
	}

	var batches [][]store.Message
	start := 0
	for start < len(messages) {
		end := start
		chars := 0
		for end < len(messages) {
			nextChars := chars + utf8.RuneCountInString(messages[end].Content)
			nextCount := end - start + 1
			if nextCount > 1 && (nextCount > segmentBatchMaxMessages || nextChars > segmentBatchMaxChars) {
				break
			}
			chars = nextChars
			end++
		}
		if end == start {
			end = start + 1
		}
		batches = append(batches, messages[start:end])
		if end >= len(messages) {
			break
		}
		// 重叠2部分
		nextStart := end - segmentBatchOverlap
		if nextStart <= start {
			nextStart = start + 1
		}
		start = nextStart
	}
	return batches
}

func mergeBatchSegments(segments []llm.Segment) []llm.Segment {
	seen := make(map[string]struct{}, len(segments))
	merged := make([]llm.Segment, 0, len(segments))
	for _, segment := range segments {
		key := fmt.Sprintf("%d:%d", segment.SeqStart, segment.SeqEnd)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, segment)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].SeqStart != merged[j].SeqStart {
			return merged[i].SeqStart < merged[j].SeqStart
		}
		return merged[i].SeqEnd < merged[j].SeqEnd
	})
	return merged
}

// #3 写入记忆
func (e *ExtractExcuter) replaceMemories(ctx context.Context, sessionID uint, memories []store.Memory) error {
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("session_id = ?", sessionID).Delete(&store.Memory{}).Error; err != nil {
			return err
		}

		if len(memories) == 0 {
			return nil
		}

		return tx.Create(&memories).Error
	})
}

// keepValidSegments 丢掉编号越界或者反向的段，并给空标题补上兜底值。
// 模型偶尔会编出范围外的编号，直接写库会产生指不回原文的记忆。
func keepValidSegments(segments []llm.Segment, messages []store.Message) []llm.Segment {
	bySeq := make(map[int]store.Message, len(messages))
	for _, msg := range messages {
		bySeq[msg.Seq] = msg
	}

	valid := make([]llm.Segment, 0, len(segments))
	for _, segment := range segments {
		if segment.SeqStart > segment.SeqEnd {
			continue
		}
		if _, ok := bySeq[segment.SeqStart]; !ok {
			continue
		}
		if _, ok := bySeq[segment.SeqEnd]; !ok {
			continue
		}

		if strings.TrimSpace(segment.Title) == "" {
			segment.Title = firstLine(bySeq[segment.SeqStart].Content)
		}
		if strings.TrimSpace(segment.Summary) == "" {
			segment.Summary = bySeq[segment.SeqStart].Content
		}

		valid = append(valid, segment)
	}

	return valid
}

func wholeSessionSegment(messages []store.Message) llm.Segment {
	first, last := messages[0], messages[len(messages)-1]

	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}

	return llm.Segment{
		SeqStart: first.Seq,
		SeqEnd:   last.Seq,
		Title:    firstLine(first.Content),
		Summary:  strings.Join(contents, "\n"),
		Tag:      string(store.TagOther),
	}
}

func allowedTags() []string {
	tags := make([]string, 0, len(store.MemoryTags))
	for _, tag := range store.MemoryTags {
		tags = append(tags, string(tag))
	}
	return tags
}

func firstLine(content string) string {
	line := strings.ReplaceAll(content, "\r\n", "\n")
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = line[:index]
	}
	return truncateTitle(line)
}

func truncateTitle(title string) string {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) <= maxTitleLength {
		return title
	}

	return string([]rune(title)[:maxTitleLength])
}
