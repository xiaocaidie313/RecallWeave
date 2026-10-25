package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"recallweave/internal/llm"
)

const ToolName = "update_plan"

const (
	Pending    = "pending"
	InProgress = "in_progress"
	Completed  = "completed"
	Blocked    = "blocked"
	Skipped    = "skipped"
)

// Step 是计划里的一步，Status/Result 由模型通过 update_plan 更新。
type Step struct {
	Step       string           `json:"step"`
	Tools      []llm.ToolSchema `json:"tools,omitempty"`
	Status     string           `json:"status"`
	Result     string           `json:"result,omitempty"`
	Confidence float64          `json:"confidence,omitempty"`
}

// Plan 是模型维护的任务计划。步骤状态和结果是模型报告的进度，实际工具执行另记在 RunState.Steps。
type Plan struct {
	Goal       string  `json:"goal"`
	Steps      []Step  `json:"steps"`
	Conclusion string  `json:"conclusion,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
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
	for i := range p.Steps {
		if p.Steps[i].Status == "" {
			p.Steps[i].Status = Pending
		}
	}
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// Valid 保留现有入口，详细原因由 Validate 返回。
func Valid(p Plan) bool {
	return p.Validate() == nil
}

func (p Plan) Validate() error {
	if strings.TrimSpace(p.Goal) == "" || len(p.Steps) == 0 || len(p.Steps) > 10 {
		return fmt.Errorf("plan: goal and 1-10 steps are required")
	}
	active := 0
	for i, step := range p.Steps {
		if strings.TrimSpace(step.Step) == "" {
			return fmt.Errorf("plan: step %d has no description", i+1)
		}
		switch step.Status {
		case Pending:
		case InProgress:
			active++
		case Completed, Blocked, Skipped:
			if strings.TrimSpace(step.Result) == "" {
				return fmt.Errorf("plan: step %d requires a result or reason for status %s", i+1, step.Status)
			}
		default:
			return fmt.Errorf("plan: step %d has invalid status %q", i+1, step.Status)
		}
	}
	if active > 1 {
		return fmt.Errorf("plan: only one step may be in_progress")
	}
	return nil
}

func (p Plan) ActiveStep() string {
	for _, step := range p.Steps {
		if step.Status == InProgress {
			return step.Step
		}
	}
	return ""
}

func (p Plan) HasOpenSteps() bool {
	for _, step := range p.Steps {
		if step.Status == Pending || step.Status == InProgress {
			return true
		}
	}
	return false
}

func (p Plan) HasUncompletedSteps() bool {
	for _, step := range p.Steps {
		if step.Status != Completed {
			return true
		}
	}
	return false
}
