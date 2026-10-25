// Package agent 只描述「一个 Agent 是谁、会什么」，执行交给 agent/runtime。
// runtime 依赖本包，本包不能反过来 import runtime。
package agent

import (
	"fmt"
	"strings"

	"recallweave/internal/llm"
	"recallweave/internal/skill"
)

const defaultMaxRounds = 6

type Agent struct {
	ID            int64
	Name          string
	Role          string
	Model         llm.ChatGenerator
	MaxRounds     int
	Abilities     []*Ability
	Skills        *skill.Catalog
	ExternalTools []llm.Tool
}

// WithExternalTools attaches selected MCP tools without turning them into an
// Ability or Skill. Names must not shadow existing tools.
func (a *Agent) WithExternalTools(external []llm.Tool) error {
	existing := make(map[string]bool)
	for _, schema := range a.Tools().Schemas() {
		existing[schema.Name] = true
	}
	for _, tool := range external {
		if tool.Schema.Name == "" || existing[tool.Schema.Name] {
			return fmt.Errorf("duplicate or empty external tool name %q", tool.Schema.Name)
		}
		existing[tool.Schema.Name] = true
	}
	a.ExternalTools = append([]llm.Tool(nil), external...)
	return nil
}

// WithSkills attaches task instructions separately from business abilities.
func (a *Agent) WithSkills(catalog *skill.Catalog) *Agent {
	a.Skills = catalog
	return a
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

// Tools registers business abilities and the built-in Skill loader separately.
func (a *Agent) Tools() *llm.ToolHandle {
	tools := llm.NewToolHandle()
	for _, ability := range a.Abilities {
		for _, tool := range ability.Tools() {
			tools.Register(tool)
		}
	}
	if a.Skills != nil && len(a.Skills.List()) > 0 {
		for _, tool := range skillTools(a.Skills) {
			tools.Register(tool)
		}
	}
	for _, tool := range a.ExternalTools {
		tools.Register(tool)
	}
	return tools
}

// Instructions includes the role, ability guidance, and Skill metadata only.
func (a *Agent) Instructions() string {
	var b strings.Builder
	if a.Role != "" {
		fmt.Fprintf(&b, "你的角色是 %s。\n\n", a.Role)
	}
	if len(a.Abilities) > 0 {
		b.WriteString("你的能力如下：\n")
		for _, ability := range a.Abilities {
			fmt.Fprintf(&b, "- %s: %s\n", ability.Name, ability.Description)
			if ability.Instructions != "" {
				b.WriteString(ability.Instructions)
				b.WriteString("\n")
			}
		}
	}
	if a.Skills != nil {
		items := a.Skills.List()
		if len(items) > 0 {
			b.WriteString("\n可用 Skill 如下。只有用户任务匹配某个描述时才调用 load_skill，读取完整说明后再按说明处理。若说明引用 references/ 文件且确实需要，再调用 read_skill_reference；不要假装已执行未提供的工具或脚本。\n")
			for _, item := range items {
				fmt.Fprintf(&b, "- %s: %s\n", item.Name, item.Description)
			}
		}
	}
	return b.String()
}
