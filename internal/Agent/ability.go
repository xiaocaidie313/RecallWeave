package Agent

import "recallweave/internal/llm"

type ToolBox []llm.Tool


type Ability struct { 
	Name        string
	Description string
	ToolBox     ToolBox
}

func (a *Ability) Tools() []llm.Tool {
	return a.ToolBox
}
// 但是ability  需要关注tool的run吗