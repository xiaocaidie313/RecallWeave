// Package agent 只描述「一个 Agent 是谁、会什么」，执行交给 agent/runtime。
// runtime 依赖本包，本包不能反过来 import runtime。
package agent

import (
	"fmt"
	"strings"

	"recallweave/internal/llm"
)

const defaultMaxRounds = 6

type Agent struct {
	ID        int64
	Name      string
	Role      string
	Model     llm.ChatGenerator
	MaxRounds int
	Abilities []*Ability
}

func NewAgent(id int64, name string, model llm.ChatGenerator, role string, maxRounds int, abilities ...*Ability) *Agent {
	return &Agent{
		ID:        id,
		Name:      name,
		Role:      role,
		Model:     model,
		MaxRounds: maxRounds,
		Abilities: abilities,
	}
}

func (a *Agent) Rounds() int {
	if a.MaxRounds <= 0 {
		return defaultMaxRounds
	}
	return a.MaxRounds
}

// Tools 把所有 Ability 的工具注册到一个 ToolHandle。同名工具后注册的会覆盖先注册的。
func (a *Agent) Tools() *llm.ToolHandle {
	tools := llm.NewToolHandle()
	for _, ability := range a.Abilities {
		for _, tool := range ability.Tools() {
			tools.Register(tool)
		}
	}
	return tools
}

// Instructions 是 Agent 自身的 system prompt：角色 + 每个 Ability 的用法说明。
func (a *Agent) Instructions() string {
	var b strings.Builder
	if a.Role != "" {
		fmt.Fprintf(&b, "你的角色是 %s。\n\n", a.Role)
	}
	if len(a.Abilities) == 0 {
		return b.String()
	}

	b.WriteString("你的能力如下：\n")
	for _, ability := range a.Abilities {
		fmt.Fprintf(&b, "- %s: %s\n", ability.Name, ability.Description)
		if ability.Instructions != "" {
			b.WriteString(ability.Instructions)
			b.WriteString("\n")
		}
	}
	return b.String()
}
