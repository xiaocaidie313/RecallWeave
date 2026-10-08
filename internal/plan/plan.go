package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"recallweave/internal/llm"
)

// Step 是计划里的一步。Status/Result/Confidence 是运行期状态，执行时由执行器回写。
type Step struct {
	Step       string           `json:"step"`
	Tools      []llm.ToolSchema `json:"tools"`
	Status     string           `json:"status"`
	Result     string           `json:"result"`
	Confidence float64          `json:"confidence"`
}

// Plan 是一次多步任务的规划，也是 planner 交给 executor 的「单子」。
type Plan struct {
	Goal       string  `json:"goal"`
	Steps      []Step  `json:"steps"`
	Conclusion string  `json:"conclusion"`
	Confidence float64 `json:"confidence"`
}

// Parse 把模型输出解析成 Plan。
// 模型常把 JSON 包在 ```json 代码块里、前后还带废话，所以不做整段反序列化，
// 而是取第一个 '{' 到最后一个 '}' 之间的内容——围栏和废话都不含 {}，会被自然丢掉。
func Parse(raw string) (Plan, error) {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end < start {
		return Plan{}, fmt.Errorf("plan: no json object in model output")
	}

	var p Plan
	if err := json.Unmarshal([]byte(raw[start:end+1]), &p); err != nil {
		return Plan{}, fmt.Errorf("plan: invalid json: %w", err)
	}
	return p, nil
}
