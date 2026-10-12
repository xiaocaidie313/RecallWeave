package ask

import (
	"recallweave/internal/agent/ability"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
)

// NewAskToolHandle 在 ask 迁到 agent.Run 之前过渡用，工具本身由 memory ability 提供。
func NewAskToolHandle(manager *memory.MemoryManger) *llm.ToolHandle {
	tools := llm.NewToolHandle()
	for _, tool := range ability.NewMemoryAbility(manager).Tools() {
		tools.Register(tool)
	}
	return tools
}
