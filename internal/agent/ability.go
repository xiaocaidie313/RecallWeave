package agent

import "recallweave/internal/llm"

type ToolBox []llm.Tool

// Ability 是挂给 Agent 的一组工具，Instructions 会拼进 system prompt，
// 告诉模型这组工具何时、如何使用。
type Ability struct {
	Name         string
	Description  string
	Instructions string
	ToolBox      ToolBox
}

func NewAbility(name, description, instructions string, toolBox ToolBox) *Ability {
	return &Ability{
		Name:         name,
		Description:  description,
		Instructions: instructions,
		ToolBox:      toolBox,
	}
}

func (a *Ability) Tools() []llm.Tool {
	return a.ToolBox
}
