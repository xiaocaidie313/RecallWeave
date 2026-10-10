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
// 模型常把 JSON 包在 ```json 代码块里、前后还带废话，所以取第一个 '{' 到最后一个 '}'
// 之间的内容再反序列化——围栏和废话都不含 {}，会被自然丢掉。
// 解析成功后顺手校验一次，空壳计划（没有 goal 或没有 steps）直接挡在门外。
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
	if !Valid(p) {
		return Plan{}, fmt.Errorf("plan: invalid plan: empty goal or no steps")
	}
	return p, nil
}

// Valid 判断计划是否可用：目标非空且至少有一个步骤。
// true / false 是 Go 内置的布尔常量，直接 return 即可，不需要自定义类型。
func Valid(p Plan) bool {
	if strings.TrimSpace(p.Goal) != "" && len(p.Steps) > 0 {
		return true
	}
	return false
}
