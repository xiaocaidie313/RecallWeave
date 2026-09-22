package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const defaultTimeout = 30 * time.Second

// Message 是交给模型切分的一条消息。这里不用 store.Message，
// 是为了让 llm 包不依赖数据库层。
type Message struct {
	Seq     int
	Role    string
	Content string
}

// Segment 是模型切出来的一个话题段。SeqStart 到 SeqEnd 指回消息编号，
// 记忆靠它追溯原文。
type Segment struct {
	SeqStart int    `json:"seq_start"`
	SeqEnd   int    `json:"seq_end"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Tag      string `json:"tag"`
}

// Segmenter 是 extract 唯一需要的能力。定成接口，测试时可以塞假实现，
// 不用真发网络请求。
type Segmenter interface {
	Segment(ctx context.Context, messages []Message, allowedTags []string) ([]Segment, error)
}

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

type Client struct {
	model string
	sdk   openai.Client
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("llm: api key is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("llm: model is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}

	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(cfg.Timeout),
	}
	// BaseURL 给到 /v1 就够，后面的 chat/completions 由 SDK 自己补。
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	return &Client{
		model: cfg.Model,
		sdk:   openai.NewClient(opts...),
	}, nil
}

const segmentPromptTemplate = `你是一个记忆提炼助手。下面是同一段会话的消息，每条前面是编号。
请按话题把它们切成若干段，每段提炼成一条可检索的记忆。

要求：
- seq_start 和 seq_end 是该段覆盖的消息编号，必须落在 %d 到 %d 之间，且 seq_start <= seq_end
- 同一段里的消息应该在讨论同一件事，段与段之间不要重叠
- title 不超过 20 字，summary 两句以内
- tag 只能从这个列表里选：%s。选不出来就用 other
- 只输出 JSON，不要解释，不要代码块

输出格式：
{"segments":[{"seq_start":1,"seq_end":3,"title":"标题","summary":"摘要","tag":"work"}]}`

type segmentResponse struct {
	Segments []Segment `json:"segments"`
}

func (c *Client) Segment(ctx context.Context, messages []Message, allowedTags []string) ([]Segment, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	minSeq, maxSeq := messages[0].Seq, messages[0].Seq
	var builder strings.Builder
	for _, msg := range messages {
		if msg.Seq < minSeq {
			minSeq = msg.Seq
		}
		if msg.Seq > maxSeq {
			maxSeq = msg.Seq
		}
		fmt.Fprintf(&builder, "%d: (%s) %s\n", msg.Seq, msg.Role, msg.Content)
	}

	prompt := fmt.Sprintf(segmentPromptTemplate, minSeq, maxSeq, strings.Join(allowedTags, ", "))

	response, err := c.sdk.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(prompt),
			openai.UserMessage(builder.String()),
		},
		// 让模型只返回 JSON，省掉解析自由文本。
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: chat completion failed: %w", err)
	}

	if len(response.Choices) == 0 {
		return nil, fmt.Errorf("llm: response has no choices")
	}

	var parsed segmentResponse
	if err := json.Unmarshal([]byte(response.Choices[0].Message.Content), &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode segments: %w", err)
	}

	return parsed.Segments, nil
}

// 后期拓展画布能力  画图--类似思维导图

// summary 的时候应该是根据 某个 session  然后说按照seq的顺序来提炼记忆
