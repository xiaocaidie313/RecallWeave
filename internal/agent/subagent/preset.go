package subagent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// PresetAgent is trusted server configuration. Model requests can select its Name,
// but cannot alter its instructions, tool allowlist or execution budget.
type PresetAgent struct {
	Name         string
	Description  string
	Instructions string
	AllowedTools []string
	MaxRounds    int
	Timeout      time.Duration
}

var PresetAgentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DefaultPresetAgents returns new values on every call, including tool slices.
// Zero budgets inherit the Manager's configured defaults.
func DefaultPresetAgents() []PresetAgent {
	// 依旧硬编码
	return []PresetAgent{
		{
			Name:         "researcher",
			Description:  "只读研究员：检索历史或外部资料，核对原文并返回来源与未解决的问题。",
			Instructions: "你是只读研究员。独立检索、核对原文或网页，必要时调整查询；只依据实际取回的资料回答。",
			AllowedTools: []string{"search_memories", "get_messages", "get_sessions", "mcp_tavily_search", "mcp_tavily_extract"},
		},
		{
			Name:         "reviewer",
			Description:  "只读核查员：检查已有结论与给定来源是否一致，指出证据缺口和推断过度。",
			Instructions: "你是只读核查员。围绕任务中已有的结论和给定来源进行核对，区分已支持、存在冲突和证据不足的部分；明确指出未解决的缺口，不能把复核自动当成独立事实验证。",
			AllowedTools: []string{"get_messages", "mcp_tavily_extract"},
		},
	}
}

func validateBudget(rounds int, timeout time.Duration) error {
	if rounds < 1 || rounds > 20 {
		return fmt.Errorf("max_rounds must be between 1 and 20")
	}
	if timeout <= 0 || timeout > 5*time.Minute {
		return fmt.Errorf("timeout must be positive and at most 5 minutes")
	}
	return nil
}

func validatePresetAgent(PresetAgent PresetAgent) error {
	if !PresetAgentNamePattern.MatchString(PresetAgent.Name) {
		return fmt.Errorf("name must match %s", PresetAgentNamePattern.String())
	}
	if !utf8.ValidString(PresetAgent.Description) || strings.TrimSpace(PresetAgent.Description) == "" || len(PresetAgent.Description) > 500 {
		return fmt.Errorf("description must be nonempty UTF-8 and at most 500 bytes")
	}
	if !utf8.ValidString(PresetAgent.Instructions) || strings.TrimSpace(PresetAgent.Instructions) == "" || len(PresetAgent.Instructions) > 8<<10 {
		return fmt.Errorf("instructions must be nonempty UTF-8 and at most 8 KiB")
	}
	if err := validateBudget(PresetAgent.MaxRounds, PresetAgent.Timeout); err != nil {
		return err
	}
	if len(PresetAgent.AllowedTools) == 0 {
		return fmt.Errorf("allowed_tools must not be empty")
	}
	seen := make(map[string]bool, len(PresetAgent.AllowedTools))
	for _, name := range PresetAgent.AllowedTools {
		if !toolNamePattern.MatchString(name) || seen[name] {
			return fmt.Errorf("invalid or duplicate allowed tool %q", name)
		}
		seen[name] = true
		switch name {
		case "delegate_task", "followup_task", "list_subagents", "get_subagent_task", "cancel_subagent_task", "update_plan", "load_skill", "read_skill_reference", "save_artifact", "deactivate_memory":
			return fmt.Errorf("tool %q cannot be granted by a subagent PresetAgent", name)
		}
	}
	return nil
}
