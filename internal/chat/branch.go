package chat

import (
	"strings"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

func isGlobalMemoryTool(name string) bool {
	for _, native := range []string{"search_memories", "get_messages", "get_sessions", "deactivate_memory"} {
		if name == native || strings.HasPrefix(name, "mcp_") && strings.HasSuffix(name, "_"+native) {
			return true
		}
	}
	return false
}

// branchCore copies configuration, not execution state. Never mutate the shared
// core's abilities or tool closures while another conversation may be running.
// copy 能力和工具
func branchCore(core *agent.Agent, visible []store.ContextMessage) *agent.Agent {
	copy := *core
	copy.Abilities = make([]*agent.Ability, 0, len(core.Abilities)+1)
	for _, original := range core.Abilities {
		item := *original
		item.ToolBox = make(agent.ToolBox, 0, len(original.ToolBox))
		for _, tool := range original.ToolBox {
			if !isGlobalMemoryTool(tool.Schema.Name) {
				item.ToolBox = append(item.ToolBox, tool)
			}
		}
		if len(item.ToolBox) > 0 {
			copy.Abilities = append(copy.Abilities, &item)
		}
	}
	copy.Abilities = append(copy.Abilities, ability.NewBranchContextAbility(visible))
	copy.ExternalTools = make([]llm.Tool, 0, len(core.ExternalTools))
	for _, tool := range core.ExternalTools {
		if !isGlobalMemoryTool(tool.Schema.Name) {
			copy.ExternalTools = append(copy.ExternalTools, tool)
		}
	}
	return &copy
}
