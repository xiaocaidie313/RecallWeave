package extract

import (
	"strings"
	"unicode/utf8"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

// Title 字段是 varchar(191)，这里留出余量，按字符数而不是字节数截断。
const maxTitleLength = 60

type ExtractExcuter struct {
	db *gorm.DB
}

func NewExtractExcuter(db *gorm.DB) *ExtractExcuter {
	return &ExtractExcuter{db: db}
}

// ExtractSource 把一次导入的消息提炼成记忆，返回生成的条数。
func (e *ExtractExcuter) ExtractSource(sourceID uint) (int, error) {
	messages, err := e.GetMessages(sourceID)
	if err != nil {
		return 0, err
	}

	if err := e.GenerateMemory(messages); err != nil {
		return 0, err
	}

	return len(messages), nil
}

// #1 读取消息
func (e *ExtractExcuter) GetMessages(sourceID uint) ([]store.Message, error) {
	var messages []store.Message
	if err := e.db.Where("source_id = ?", sourceID).Order("seq").Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// #2 生成记忆。摘要暂时就是原文，等接入大模型后替换。
func (e *ExtractExcuter) GenerateMemory(messages []store.Message) error {
	if len(messages) == 0 {
		return nil
	}

	memories := make([]store.Memory, 0, len(messages))
	for _, msg := range messages {
		memories = append(memories, store.Memory{
			SourceID:  msg.SourceID,
			MessageID: msg.ID,
			Title:     titleOf(msg.Content),
			Summary:   msg.Content,
			Content:   msg.Content,
		})
	}

	return e.db.Create(&memories).Error
}

// titleOf 取正文第一行当标题。
func titleOf(content string) string {
	title := strings.ReplaceAll(content, "\r\n", "\n")
	if index := strings.IndexByte(title, '\n'); index >= 0 {
		title = title[:index]
	}

	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) <= maxTitleLength {
		return title
	}

	return string([]rune(title)[:maxTitleLength])
}

//@todo 记忆存储分层----- L1 L2 L3  类似于缓存机制
// #3 接入大模型生成真正的摘要和标签
