package llm

import (
	"time"

	openai "github.com/openai/openai-go/v3"
)

// Conversation 持有一轮问答的消息历史。SDK 的消息类型只出现在这里，
// 上层看见的都是普通字符串。
// ChatCompletionMessageParamUnion 包裹任意一种角色的信息
type Conversation struct {
	ID        uint
	Title     string
	messages  []openai.ChatCompletionMessageParamUnion
	CreatedAt time.Time
}

type ConvOption func(*Conversation)

func WithConversationID(id uint) ConvOption {
	return func(c *Conversation) {
		c.ID = id
	}
}

func WithConversationTitle(title string) ConvOption {
	return func(c *Conversation) {
		c.Title = title
	}
}

// 开启新对话 注入system prompt
func NewConversation(systemPrompt string, opts ...ConvOption) *Conversation {
	conv := &Conversation{
		ID:        999999,
		Title:     "",
		CreatedAt: time.Now(),
		messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
		},
	}
	for _, opt := range opts {
		opt(conv)
	}
	return conv
}

func (conv *Conversation) AddUserMessage(content string) {
	conv.messages = append(conv.messages, openai.UserMessage(content)) // system, userMessage, assistantMessage, toolMessage, functionMessage
}

// AddSystemMessage 追加一条 system 消息。system 消息用于放背景资料，
// 比如预取的记忆摘要——那是环境信息，不是用户说的话。
func (conv *Conversation) AddSystemMessage(content string) {
	conv.messages = append(conv.messages, openai.SystemMessage(content))
}

func (conv *Conversation) AddAssistantMessage(content string) {
	conv.messages = append(conv.messages, openai.AssistantMessage(content))
}

// AddToolResult 把工具执行结果回灌给模型。toolCallID 必须和请求时的 ID 对上，
// 否则模型不知道这是哪个调用的结果。
func (conv *Conversation) AddToolResult(toolCallID, content string) {
	conv.messages = append(conv.messages, openai.ToolMessage(content, toolCallID))
}
