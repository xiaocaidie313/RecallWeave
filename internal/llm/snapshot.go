package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

const (
	ConversationSnapshotVersion     = 1
	MaxConversationSnapshotBytes    = 512 << 10
	MaxConversationSnapshotMessages = 400
)

// ConversationSnapshot is an opaque, versioned copy of the model's request
// messages. SDK parameter types remain inside llm. It contains the actual tool
// calls and replies, not a model-generated summary.
type ConversationSnapshot struct {
	Version  int             `json:"version"`
	Messages json.RawMessage `json:"messages"`
}

// Snapshot only accepts complete tool batches. A partial batch cannot safely
// be replayed, because some tools may already have executed before cancellation.
func (conv *Conversation) Snapshot() (*ConversationSnapshot, error) {
	if conv == nil {
		return nil, fmt.Errorf("llm: cannot snapshot a nil conversation")
	}
	messages, err := json.Marshal(conv.messages)
	if err != nil {
		return nil, fmt.Errorf("llm: encode conversation snapshot: %w", err)
	}
	// json 化的 conversation
	snapshot := &ConversationSnapshot{Version: ConversationSnapshotVersion, Messages: messages}
	// 验证对话切片
	if _, err := validateConversationSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// RestoreConversation replaces the first system message with current policy,
// while preserving all remaining messages, including original tool-call ids
// and result strings. Restoring never invokes any tool.
func RestoreConversation(systemPrompt string, snapshot *ConversationSnapshot, opts ...ConvOption) (*Conversation, error) {
	if !utf8.ValidString(systemPrompt) {
		return nil, fmt.Errorf("llm: system prompt must be UTF-8")
	}
	messages, err := validateConversationSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	// 创建新对话  复活原对话
	conv := NewConversation(systemPrompt, opts...)
	for _, raw := range messages[1:] {
		// Raw overrides are a supported SDK parameter surface. Keeping their JSON
		// avoids dropping optional fields while rebuilding SDK union structs.
		copy := append(json.RawMessage(nil), raw...)
		// 添加到新对话
		conv.messages = append(conv.messages, param.Override[openai.ChatCompletionMessageParamUnion](copy))
	}
	if _, err := conv.Snapshot(); err != nil {
		return nil, err
	}
	return conv, nil
}

// AddAssistantTurn lets a custom ChatGenerator obey Next's existing contract:
// add its returned assistant turn before the runtime appends tool results.
// Client.Next already adds the SDK response and does not call this helper.
func (conv *Conversation) AddAssistantTurn(turn Turn) {
	// 包装成assistant 消息
	message := openai.AssistantMessage(turn.Content)
	for _, call := range turn.ToolCalls {
		// 伪造真的 工具调用
		message.OfAssistant.ToolCalls = append(message.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID:       call.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: call.Name, Arguments: call.Arguments},
			},
		})
	}
	// 添加到新对话
	conv.messages = append(conv.messages, message)
}

type snapshotMessage struct {
	Role       string             `json:"role"`
	Content    json.RawMessage    `json:"content,omitempty"`
	Name       *string            `json:"name,omitempty"`
	Refusal    *string            `json:"refusal,omitempty"`
	ToolCalls  []snapshotToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

// 定义 快照工具调用
type snapshotToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string  `json:"name"`
		Arguments *string `json:"arguments"`
	} `json:"function"`
}

// 验证 对话快照
func validateConversationSnapshot(snapshot *ConversationSnapshot) ([]json.RawMessage, error) {
	// 检验版本
	if snapshot == nil || snapshot.Version != ConversationSnapshotVersion {
		return nil, fmt.Errorf("llm: unsupported conversation snapshot version")
	}
	if len(snapshot.Messages) > MaxConversationSnapshotBytes || !utf8.Valid(snapshot.Messages) {
		return nil, fmt.Errorf("llm: conversation snapshot must be UTF-8 and at most 512 KiB")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > MaxConversationSnapshotBytes {
		return nil, fmt.Errorf("llm: conversation snapshot must be valid JSON and at most 512 KiB")
	}
	// 检验重复键
	if err := rejectDuplicateKeys(snapshot.Messages); err != nil {
		return nil, fmt.Errorf("llm: invalid conversation snapshot JSON: %w", err)
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(snapshot.Messages, &messages); err != nil || len(messages) == 0 || len(messages) > MaxConversationSnapshotMessages {
		return nil, fmt.Errorf("llm: conversation snapshot needs 1 to 400 messages")
	}
	pending := make(map[string]bool)
	for index, raw := range messages {
		var message snapshotMessage

		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&message); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("llm: invalid snapshot message %d: %w", index, err)
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || !snapshotFieldsAllowed(message.Role, fields) {
			return nil, fmt.Errorf("llm: invalid snapshot fields at message %d", index)
		}
		if _, present := fields["name"]; present && message.Name == nil {
			return nil, fmt.Errorf("llm: invalid snapshot participant name at message %d", index)
		}
		if _, present := fields["refusal"]; present && message.Refusal == nil {
			return nil, fmt.Errorf("llm: invalid snapshot refusal at message %d", index)
		}
		if index == 0 && message.Role != "system" {
			return nil, fmt.Errorf("llm: snapshot must begin with system policy")
		}
		// 如果还有没扫完的工具 但是 不是tool 角色 说明中断
		if len(pending) != 0 && message.Role != "tool" {
			return nil, fmt.Errorf("llm: snapshot message %d interrupts an unfinished tool batch", index)
		}
		if message.Name != nil && (!utf8.ValidString(*message.Name) || strings.TrimSpace(*message.Name) == "") {
			return nil, fmt.Errorf("llm: invalid snapshot participant name at message %d", index)
		}
		switch message.Role {
		case "system", "user":
			if message.Refusal != nil || message.ToolCallID != "" || len(message.ToolCalls) != 0 || !snapshotText(message.Content, true) {
				return nil, fmt.Errorf("llm: invalid %s snapshot message %d", message.Role, index)
			}
		case "assistant":
			if message.ToolCallID != "" || (len(message.Content) != 0 && !snapshotText(message.Content, false)) {
				return nil, fmt.Errorf("llm: invalid assistant snapshot message %d", index)
			}
			if len(message.ToolCalls) == 0 && !snapshotNonemptyText(message.Content) && (message.Refusal == nil || *message.Refusal == "") {
				return nil, fmt.Errorf("llm: empty assistant snapshot message %d", index)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || pending[call.ID] || call.Type != "function" || call.Function.Name == "" || call.Function.Arguments == nil {
					return nil, fmt.Errorf("llm: invalid or duplicate tool call at snapshot message %d", index)
				}
				pending[call.ID] = true
			}
		case "tool":
			if message.Name != nil || message.Refusal != nil || len(message.ToolCalls) != 0 || !pending[message.ToolCallID] || !snapshotText(message.Content, true) {
				return nil, fmt.Errorf("llm: unmatched tool result at snapshot message %d", index)
			}
			delete(pending, message.ToolCallID)
		default:
			return nil, fmt.Errorf("llm: unsupported snapshot role at message %d", index)
		}
	}

	// 一整轮工具都 AddToolResult 完，或模型直接给了最终文本（没有未闭合的 tool_calls），校验通过，快照才有值。主问答 CaptureContext 为 false，根本不拍。
	if len(pending) != 0 {
		return nil, fmt.Errorf("llm: conversation snapshot has unfinished tool calls")
	}
	return messages, nil
}

func snapshotFieldsAllowed(role string, fields map[string]json.RawMessage) bool {
	allowed := map[string]bool{"role": true, "content": true}
	// 根据不同角色 限制不同 key
	switch role {
	case "system", "user":
		allowed["name"] = true
	case "assistant":
		allowed["name"], allowed["refusal"], allowed["tool_calls"] = true, true, true
	case "tool":
		allowed["tool_call_id"] = true
	default:
		return false
	}
	for field := range fields {
		if !allowed[field] {
			return false
		}
	}
	return true
}

func snapshotText(raw json.RawMessage, required bool) bool {
	if len(raw) == 0 {
		return !required
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var text string
	return json.Unmarshal(raw, &text) == nil && utf8.ValidString(text)
}

func snapshotNonemptyText(raw json.RawMessage) bool {
	var text string
	return json.Unmarshal(raw, &text) == nil && text != ""
}

// The standard JSON decoder accepts duplicate keys. Snapshots must not have
// multiple interpretations depending on which decoder later consumes them.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber() // 将 float64 转换为 json.Number 类型
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		// 解码器返回的 token 是 json.Delim 类型
		// 每一小块
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		// 进入对象
		case '{':
			seen := make(map[string]bool)
			for decoder.More() { // 是否有更多数据
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] { // 在同一{} 下 键不能重复
					return fmt.Errorf("duplicate or invalid object key")
				}
				seen[key] = true               // 去重
				if err := walk(); err != nil { // 递归读取键值对
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

func DecodeDisallowUnknownFields[T any](data []byte) (T, error) {
	var v T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&v); err != nil {
		return v, err
	}
	return v, nil
}
