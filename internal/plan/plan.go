import (
	"recallweave/internal/llm"
)

type Plan {
	Goal string `json:"goal"`
	steps [] step
}

type step {
	step 
	tools []llm.ToolSchema
	result string
	confidence float64
	status string
}
