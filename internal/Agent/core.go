// @todo muti-agent  可以写成core + ability(多个tool? 升级合并能力？)
// 目前的代码只是实现了流水线? 还不是agent
package Agent

import (
	"context"
	"recallweave/internal/llm"
)

type ToolBox []llm.ToolSchema

type Agent struct {
	ID            int64
	Name          string
	chatGenerator llm.ChatGenerator //开启对话
	role          string
	// abilities
}

type RunContext struct {
	RunId          string              `json:"run_id"`          // 每次执行run编号
	ConversationID string              `json:"conversation_id"` // 在哪次对话触发的
	Input          string              // 因为什么input开始执行的
	RecordStep     []map[string]string // 记录整个run的过程用了什么tool
	status         []map[string]string // 这个run过程的状态
	errors         []map[string]string // 过程有问题先收集 不打断  // 先留着 还不知道要不要
}

func NewAgent(ID int64, Name string, chatGenerator llm.ChatGenerator, role string) *Agent {
	return &Agent{
		ID:            ID,
		chatGenerator: chatGenerator,
		Name:          Name,
		role:          role,
	}
}

func (a *Agent) Run(ctx context.Context, systemPrompt string, request string) {
	// 00 检查初始环境
	if a.chatGenerator == nil {
		return // 先留着 不知道返回啥  应该是fatal 之类的吧  不返回
	}
	// 01 初始化
	// 01.1 基本对话能力  1 创建conversation
	conversation := llm.NewConversation(systemPrompt)
	// 02 准备 1 预加载历史上下文
	// 03 执行
	// 04 检查
	// 05 输出结果

}
