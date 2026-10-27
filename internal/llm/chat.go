package llm

import (
	"context"
	"fmt"

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

// Usage 是这一轮模型调用的 token 用量。没返回用量时全是 0。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
}

// Turn 是模型一轮的输出：要么给出回答，要么要求调用工具。记录会话内容/ 工具调用情况
type Turn struct {
	Content   string
	ToolCalls []ToolCall // 想要调用的tool清单
	Usage     Usage
}

// ChatGenerator 是 chat 循环依赖的能力，测试时可以塞假实现。
type ChatGenerator interface {
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

	turn := Turn{
		Content: message.Content,
		Usage:   usageFromCompletion(response.Usage),
	}
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

func usageFromCompletion(usage openai.CompletionUsage) Usage {
	return Usage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
		CachedTokens:     usage.PromptTokensDetails.CachedTokens,
	}
}

func (c *Client) ModelName() string {
	if c == nil {
		return ""
	}
	return c.model
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
