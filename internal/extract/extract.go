package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"recallweave/internal/llm"
	"recallweave/internal/types"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
// 包含停用记忆的会话禁止整体重写，以免重新激活旧内容；仍可增量提炼。
func (e *ExtractExcuter) ExtractSessionToMemory(ctx context.Context, sessionID uint) (int, error) {
	if err := e.checkGlobalExtraction(ctx, sessionID); err != nil {
		e.markExtract(ctx, sessionID, err)
		return 0, err
	}
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

	memories := e.generateMemoryFromMessage(ctx, sessionID, messages)
	if err := e.replaceMemories(ctx, sessionID, memories); err != nil {
		return 0, err
	}

	return len(memories), nil
}

// ExtractIncrementalToMemory 只提炼上次覆盖序号之后的新消息，已有记忆保留。
// 手动的 ExtractSessionToMemory 仍会删掉旧记忆再整段重写。
// 摘要失败做降级 ----> llm 自动重新提炼信息
func (e *ExtractExcuter) ExtractIncrementalToMemory(ctx context.Context, sessionID uint) (int, error) {
	if err := e.checkGlobalExtraction(ctx, sessionID); err != nil {
		e.markExtract(ctx, sessionID, err)
		return 0, err
	}
	count, err := e.extractIncrementalToMemory(ctx, sessionID)
	e.markExtract(ctx, sessionID, err)
	return count, err
}

func (e *ExtractExcuter) extractIncrementalToMemory(ctx context.Context, sessionID uint) (int, error) {
	// 某一session的message
	messages, err := e.GetMessages(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(messages) == 0 {
		return 0, nil
	}
	// 获取覆盖的序号 获取最后一条记忆的序号 获得最大seq序号
	covered, err := e.coveredSeq(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	// 获取新的消息  @todo是不是有点浪费？ 获取的session下全部message 但是只要大于 covered  极端情况就只有一条
	fresh := make([]types.Message, 0, len(messages))
	for _, msg := range messages {
		// 如果消息的序号大于覆盖的序号，则添加到新的消息中
		if msg.Seq > covered {
			fresh = append(fresh, msg)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	// fresh里存的是全新的message
	// 生成记忆从message
	memories := e.generateMemoryFromMessage(ctx, sessionID, fresh)
	// 写入记忆
	if err := e.db.WithContext(ctx).Create(&memories).Error; err != nil {
		return 0, err
	}
	return len(memories), nil
}

// 标记提取状态
func (e *ExtractExcuter) markExtract(ctx context.Context, sessionID uint, extractErr error) {
	if e.db == nil {
		return
	}
	updates := map[string]any{
		"extract_error": "",
	}
	if errors.Is(extractErr, types.ErrBranchMemoryPrivate) {
		updates["extract_status"] = types.ExtractSkipped
	} else if extractErr != nil {
		updates["extract_status"] = types.ExtractFailed
		updates["extract_error"] = extractErr.Error()
	} else {
		now := time.Now()
		updates["extract_status"] = types.ExtractDone
		updates["extracted_at"] = now
	}
	if err := e.db.WithContext(ctx).Model(&types.Session{}).Where("id = ?", sessionID).Updates(updates).Error; err != nil {
		slog.Warn("mark extract status failed", "session_id", sessionID, "error", err)
	}
}

// 获取覆盖的序号 获取最后一条记忆的序号。即最大序号
func (e *ExtractExcuter) coveredSeq(ctx context.Context, sessionID uint) (int, error) {
	var row struct {
		Seq int
	}
	err := e.db.WithContext(ctx).Model(&types.Memory{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq_end), 0) AS seq").
		Scan(&row).Error
	return row.Seq, err
}

// 生成记忆从message 通过将大段message 切断成多段segment 每段segment 生成一条记忆
func (e *ExtractExcuter) generateMemoryFromMessage(ctx context.Context, sessionID uint, messages []types.Message) []types.Memory {
	segments := e.segment(ctx, messages)
	memories := make([]types.Memory, 0, len(segments))
	for _, segment := range segments {
		memories = append(memories, types.Memory{
			SessionID:   sessionID,
			SeqStart:    segment.SeqStart,
			SeqEnd:      segment.SeqEnd,
			Title:       truncateTitle(segment.Title),
			Content:     segment.Summary,
			Tag:         types.NormalizeMemoryTag(segment.Tag),
			ContentType: types.NormalizeContentType(segment.ContentType),
			Embedding:   types.NewEmbedding(e.embedMemory(ctx, segment.Title, segment.Summary)),
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
	if len(vector) != types.ModelEmbeddingDim && len(vector) != types.EmbeddingDim {
		slog.Warn("unexpected embedding dimension", "got", len(vector), "want", types.ModelEmbeddingDim)
		return nil
	}
	return vector
}

// #1 读取消息
func (e *ExtractExcuter) GetMessages(ctx context.Context, sessionID uint) ([]types.Message, error) {
	var messages []types.Message

	if err := e.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("seq").Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// #2 切段。长会话按批交给模型，单批失败只降级该批。

func (e *ExtractExcuter) segment(ctx context.Context, messages []types.Message) []llm.Segment {
	if len(messages) == 0 {
		return nil
	}

	var segments []llm.Segment
	for _, batch := range splitMessageBatches(messages) {
		segments = append(segments, e.segmentBatch(ctx, batch)...)
	}
	return mergeBatchSegments(segments)
}

func (e *ExtractExcuter) segmentBatch(ctx context.Context, messages []types.Message) []llm.Segment {
	fallback := []llm.Segment{wholeSessionSegment(messages)} // 把 原文直接转化格式成segment
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

	segments, err := e.segmenter.Segment(ctx, input, allowedTags(), allowedContentTypes())
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

// 分割消息批次 一组要么条数大于1且大于最大条数 要么 字符数大于最大字符数
func splitMessageBatches(messages []types.Message) [][]types.Message {
	if len(messages) == 0 {
		return nil
	}

	var batches [][]types.Message
	start := 0
	for start < len(messages) {
		end := start
		chars := 0
		for end < len(messages) {
			// 计算下一组消息的字符数
			nextChars := chars + utf8.RuneCountInString(messages[end].Content)
			// 计算下一组消息的条数
			nextCount := end - start + 1
			// 如果下一组消息的条数大于1且大于最大条数或字符数大于最大字符数，则结束
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
func (e *ExtractExcuter) replaceMemories(ctx context.Context, sessionID uint, memories []types.Memory) error {
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []types.Memory
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id = ?", sessionID).Order("id").Find(&existing).Error; err != nil {
			return err
		}
		for _, item := range existing {
			if item.Status != "" && item.Status != types.MemoryActive {
				return types.ErrMemoryHistoryProtected
			}
		}
		if err := tx.Where("session_id = ?", sessionID).Delete(&types.Memory{}).Error; err != nil {
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
func keepValidSegments(segments []llm.Segment, messages []types.Message) []llm.Segment {
	bySeq := make(map[int]types.Message, len(messages))
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
		segment.ContentType = string(types.NormalizeContentType(segment.ContentType))

		valid = append(valid, segment)
	}

	return valid
}

func wholeSessionSegment(messages []types.Message) llm.Segment {
	first, last := messages[0], messages[len(messages)-1]

	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}

	return llm.Segment{
		SeqStart:    first.Seq,
		SeqEnd:      last.Seq,
		Title:       firstLine(first.Content),
		Summary:     strings.Join(contents, "\n"),
		Tag:         string(types.TagOther),
		ContentType: string(types.ContentOther),
	}
}

func allowedTags() []string {
	tags := make([]string, 0, len(types.MemoryTags))
	for _, tag := range types.MemoryTags {
		tags = append(tags, string(tag))
	}
	return tags
}

func allowedContentTypes() []string {
	allowed := make([]string, 0, len(types.ContentTypes))
	for _, contentType := range types.ContentTypes {
		allowed = append(allowed, string(contentType))
	}
	return allowed
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
