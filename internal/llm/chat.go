package llm

import (
	"context"
	"fmt"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// ToolCall 是模型这一轮想调用的工具。Arguments 是模型生成的 JSON 字符串，
// 不保证合法，执行前必须校验。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Turn 是模型一轮的输出：要么给出回答，要么要求调用工具。记录会话内容/ 工具调用情况
type Turn struct {
	Content   string
	ToolCalls []ToolCall
}

// Conversation 持有一轮问答的消息历史。SDK 的消息类型只出现在这里，
// 上层看见的都是普通字符串。
// ChatCompletionMessageParamUnion 包裹任意一种角色的信息
type Conversation struct {
	ConversationID uint
	Title          string
	messages       []openai.ChatCompletionMessageParamUnion
	CreatedAt      time.Time
}

// 开启新对话 注入system prompt
func NewConversation(systemPrompt string, conversationID uint) *Conversation {
	return &Conversation{
		ConversationID: conversationID,
		Title:          "",
		CreatedAt:      time.Now(),
		messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
		},
	}
}

func (conv *Conversation) AddUserMessage(content string) {
	conv.messages = append(conv.messages, openai.UserMessage(content)) // system, userMessage, assistantMessage, toolMessage, functionMessage
}

func (conv *Conversation) AddAssistantMessage(content string) {
	conv.messages = append(conv.messages, openai.AssistantMessage(content))
}

// AddToolResult 把工具执行结果回灌给模型。toolCallID 必须和请求时的 ID 对上，
// 否则模型不知道这是哪个调用的结果。
func (conv *Conversation) AddToolResult(toolCallID, content string) {
	conv.messages = append(conv.messages, openai.ToolMessage(content, toolCallID))
}

// Responder 是 ask 循环依赖的能力，测试时可以塞假实现。
type Responder interface {
	Next(ctx context.Context, conv *Conversation, tools []ToolSchema) (Turn, error)
	NewOneTurnChat(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// Next 请求模型的下一轮输出，并把模型这轮的回复追加进对话历史。
// 历史里必须保留带 tool_calls 的那条 assistant 消息，否则下一轮回灌结果时
// 模型会因为找不到对应的调用而报错。
func (c *Client) Next(ctx context.Context, conv *Conversation, tools []ToolSchema) (Turn, error) {
	// 模型请求入参包装
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(c.model),
		Messages: conv.messages,
	}
	// 将自定义的 tool 转换成 模型要求的格式的tool param
	for _, tool := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        tool.Name,
					Description: param.NewOpt(tool.Description),
					Parameters:  shared.FunctionParameters(tool.Parameters),
				},
			},
		})
	}
	// 模型新对话
	response, err := c.sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return Turn{}, fmt.Errorf("llm: chat completion failed: %w", err)
	}

	if len(response.Choices) == 0 {
		return Turn{}, fmt.Errorf("llm: response has no choices")
	}
	// 模型回复信息
	message := response.Choices[0].Message
	conv.messages = append(conv.messages, message.ToParam()) // toparam 把模型回复信息转换为param类型

	turn := Turn{Content: message.Content} // 记录这一轮对话的内容
	for _, call := range message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			continue
		}
		// 记录这一轮对话的工具调用
		turn.ToolCalls = append(turn.ToolCalls, ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}

	return turn, nil
}

func (c *Client) NewOneTurnChat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
	}
	response, err := c.sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("llm: chat completion failed: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("llm: response has no choices")
	}
	return response.Choices[0].Message.Content, nil
}
