package runtime

import (
	"strings"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
)

// Preload 是调用方在运行前准备好的背景和历史，Run 只负责按顺序写进对话。
type Preload struct {
	Background []string      // 预载记忆、对话简报，作为 system 消息
	History    []llm.Message // 之前的对话轮次，按 Role 还原
}

// buildConversation 按固定顺序组装对话：
// Agent 说明 + 本次 SystemPrompt → 背景 → 历史 → 本次输入。
func buildConversation(a *agent.Agent, req Request) *llm.Conversation {
	systemPrompt := strings.TrimSpace(a.Instructions() + "\n\n" + req.SystemPrompt)
	conv := llm.NewConversation(systemPrompt, llm.WithConversationID(req.ConversationID))

	for _, text := range req.Preload.Background {
		conv.AddSystemMessage(text)
	}
	for _, msg := range req.Preload.History {
		switch msg.Role {
		case "assistant":
			conv.AddAssistantMessage(msg.Content)
		case "user":
			conv.AddUserMessage(msg.Content)
		}
	}

	conv.AddUserMessage(req.Input)
	return conv
}
