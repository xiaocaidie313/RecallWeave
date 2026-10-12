package runtime

import "recallweave/internal/llm"

// Preload 是调用方在运行前准备好的背景和历史，Run 只负责按顺序写进对话。
type Preload struct {
	Background []string      // 预载记忆、对话简报，作为 system 消息
	History    []llm.Message // 之前的对话轮次，按 Role 还原
}

func applyPreload(conv *llm.Conversation, preload Preload) {
	for _, text := range preload.Background {
		conv.AddSystemMessage(text)
	}
	for _, msg := range preload.History {
		switch msg.Role {
		case "assistant":
			conv.AddAssistantMessage(msg.Content)
		case "user":
			conv.AddUserMessage(msg.Content)
		}
	}
}
