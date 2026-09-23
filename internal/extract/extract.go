package extract

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"recallweave/internal/llm"
	"recallweave/internal/store"

	"gorm.io/gorm"
)

// Title 字段是 varchar(191)，这里留出余量，按字符数而不是字节数截断。
const maxTitleLength = 60

type ExtractExcuter struct {
	db        *gorm.DB
	segmenter llm.Segmenter
}

// segmenter 传 nil 时退回本地兜底：整个会话算一段，标题取第一条消息的第一行。
func NewExtractExcuter(db *gorm.DB, segmenter llm.Segmenter) *ExtractExcuter {
	return &ExtractExcuter{db: db, segmenter: segmenter}
}

// ExtractSession 把一个会话提炼成若干条记忆，返回生成的条数。
// 重复提炼同一个会话是幂等的：先删掉旧记忆，再写新的。
func (e *ExtractExcuter) ExtractSession(ctx context.Context, sessionID uint) (int, error) {
	messages, err := e.GetMessages(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	if len(messages) == 0 {
		return 0, nil
	}

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
		})
	}

	if err := e.replaceMemories(ctx, sessionID, memories); err != nil {
		return 0, err
	}

	return len(memories), nil
}

// #1 读取消息
func (e *ExtractExcuter) GetMessages(ctx context.Context, sessionID uint) ([]store.Message, error) {
	var messages []store.Message

	if err := e.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("seq").Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// #2 切段。模型不可用或者返回不可信时，退回整段兜底，保证链路不断。
func (e *ExtractExcuter) segment(ctx context.Context, messages []store.Message) []llm.Segment {
	fallback := []llm.Segment{wholeSessionSegment(messages)}

	// 模型失效
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
		slog.Warn("segment session failed, falling back to whole session",
			"session_id", messages[0].SessionID,
			"error", err,
		)
		return fallback
	}

	valid := keepValidSegments(segments, messages)
	if len(valid) == 0 {
		slog.Warn("model returned no usable segment, falling back to whole session",
			"session_id", messages[0].SessionID,
		)
		return fallback
	}

	return valid
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

//@todo 记忆存储分层----- L1 L2 L3  类似于缓存机制
//@todo 长会话要分批交给模型，现在是一次全塞进去
