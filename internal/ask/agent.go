package ask

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/extract"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"
)

var ErrNoModel = errors.New("ask: llm is not configured")

const defaultConversationTitle = "新对话"
const maxConversationTitle = 60
const keepRecentMessages = 20

const titlePrompt = `根据用户的问题给这段对话起一个简短标题。只返回标题本身，不要解释，不要加引号。`

const briefPrompt = `把对话压缩成一份可供后续继续聊天使用的摘要。保留决定、偏好、未完成的事和关键事实。只返回摘要本身，不要解释。`

// systemPrompt 是问答场景的通用规则。身份由 Agent 的 Role 给出，
// 记忆工具的用法由 memory ability 的 Instructions 给出，这里不再重复。
const systemPrompt = `普通问题直接回答，不要为了检索而检索。

当问题需要多步才能回答（对比多个事件、跨多次检索、汇总一段经历）时，
先简要说出你的计划（一两句话），再按计划执行，最后下结论。
简单问题不要规划，直接回答，别为了规划而规划。`

type Answer struct {
	Text      string     `json:"answer"`
	Citations []Citation `json:"citations"`
	Rounds    int        `json:"rounds"`
}

// Agent 是问答服务：准备上下文 → 交给 core 跑 → 保存、提炼、压缩、起标题。
// 真正决定调哪个工具的是 core（agent.Agent），这里的步骤每次都固定执行。
type Agent struct {
	core      *agent.Agent
	memory    *memory.MemoryManger
	extracter *extract.ExtractExcuter
}

// core 或 core.Model 为 nil 表示没配 api_key，这时问答直接报错。
// 提炼有本地兜底，问答没有——没有模型就没法组织回答。
func NewAgent(core *agent.Agent, memoryManger *memory.MemoryManger, extracter *extract.ExtractExcuter) *Agent {
	return &Agent{core: core, memory: memoryManger, extracter: extracter}
}

func (a *Agent) model() llm.ChatGenerator {
	if a.core == nil {
		return nil
	}
	return a.core.Model
}

func (a *Agent) Ask(ctx context.Context, question string, conversationID uint) (Answer, error) {
	if a.model() == nil {
		return Answer{}, ErrNoModel
	}

	preload, sessionID, err := a.loadContext(ctx, conversationID, question)
	if err != nil {
		return Answer{}, err
	}
	// 请求结束后上下文会被取消，标题生成改用不会跟着取消的上下文，并写回数据库。
	go a.generateTitle(context.WithoutCancel(ctx), conversationID, question)

	result, err := runtime.Run(ctx, a.core, runtime.Request{
		Input:          question,
		ConversationID: conversationID,
		SystemPrompt:   systemPrompt,
		Preload:        preload,
	})
	logToolErrors(result.State)
	if err != nil {
		return Answer{}, err
	}

	if err := a.saveTurn(ctx, sessionID, question, result.Content); err != nil {
		return Answer{}, err
	}
	a.extractSession(ctx, sessionID)
	a.condenseWindow(ctx, conversationID, sessionID)

	return Answer{
		Text:      result.Content,
		Citations: citationsFromSteps(result.State.Steps),
		Rounds:    result.State.CurrentRound,
	}, nil
}

// logToolErrors 把运行中可恢复的工具失败记下来。这些失败已经回灌给模型，不影响回答。
func logToolErrors(state runtime.RunState) {
	for _, e := range state.Errors {
		if e.Fatal {
			continue
		}
		slog.Warn("tool call failed",
			"run_id", state.RunID,
			"round", e.Round,
			"tool", e.ToolName,
			"error", e.Message,
		)
	}
}

// extractSession 在这一问已经写入会话之后，只提炼还没覆盖到的新消息。
// 没配提炼器、或这次没有会话时跳过。失败只记日志，不把已经生成的回答变成 500。
func (a *Agent) extractSession(ctx context.Context, sessionID uint) {
	if a.extracter == nil || sessionID == 0 {
		return
	}
	if _, err := a.extracter.ExtractNewToMemory(ctx, sessionID); err != nil {
		slog.Warn("extract session failed",
			"session_id", sessionID,
			"error", err,
		)
	}
}

// condenseWindow 在消息超过最近窗口时，把更早的原文收成摘要。
// 最近的消息保持原文。失败只记日志。
func (a *Agent) condenseWindow(ctx context.Context, conversationID, sessionID uint) {
	model := a.model()
	if a.memory == nil || model == nil || conversationID == 0 || sessionID == 0 {
		return
	}

	messages, err := a.memory.ListMessages(ctx, sessionID)
	if err != nil {
		slog.Warn("condense conversation failed", "conversation_id", conversationID, "error", err)
		return
	}
	if len(messages) <= keepRecentMessages {
		return
	}

	// 已有简报 + 截止序号。untilSeq 之前的消息已被压缩过，不再重复压缩。
	brief, untilSeq, err := a.memory.ConversationBrief(ctx, conversationID)
	if err != nil {
		slog.Warn("condense conversation failed", "conversation_id", conversationID, "error", err)
		return
	}

	// 获取超过 window 的消息
	keepFrom := messages[len(messages)-keepRecentMessages].Seq
	fresh := make([]store.Message, 0)
	for _, msg := range messages {
		if msg.Seq < keepFrom && msg.Seq > untilSeq {
			fresh = append(fresh, msg)
		}
	}
	if len(fresh) == 0 {
		return
	}
	// 压缩对话得到 简报
	next, err := model.NewOneTurnChat(ctx, briefPrompt, briefInput(brief, fresh))
	if err != nil {
		slog.Warn("condense conversation failed", "conversation_id", conversationID, "error", err)
		return
	}
	next = strings.TrimSpace(next)
	if next == "" {
		return
	}
	if err := a.memory.UpdateConversationBrief(ctx, conversationID, next, fresh[len(fresh)-1].Seq); err != nil {
		slog.Warn("condense conversation failed", "conversation_id", conversationID, "error", err)
	}
}

func briefInput(brief string, messages []store.Message) string {
	var builder strings.Builder
	if brief != "" {
		builder.WriteString("已有摘要：\n")
		builder.WriteString(brief)
		builder.WriteString("\n\n新增对话：\n")
	}
	for _, msg := range messages {
		builder.WriteString(msg.Role)
		builder.WriteString(": ")
		builder.WriteString(msg.Content)
		builder.WriteByte('\n')
	}
	// 一次直接取出  缓存区的String
	return builder.String()
}

// generateTitle 只在标题还是默认值时，用第一问生成标题并写回 conversations。
func (a *Agent) generateTitle(ctx context.Context, conversationID uint, question string) {
	model := a.model()
	if a.memory == nil || model == nil || conversationID == 0 {
		return
	}

	current, err := a.memory.ConversationTitle(ctx, conversationID)
	if err != nil {
		slog.Warn("generate title failed", "conversation_id", conversationID, "error", err)
		return
	}
	if current != "" && current != defaultConversationTitle {
		return
	}

	title, err := model.NewOneTurnChat(ctx, titlePrompt, question)
	if err != nil {
		slog.Warn("generate title failed", "conversation_id", conversationID, "error", err)
		return
	}
	title = cleanTitle(title)
	if title == "" {
		return
	}
	if err := a.memory.UpdateConversationTitle(ctx, conversationID, title); err != nil {
		slog.Warn("generate title failed", "conversation_id", conversationID, "error", err)
	}
}

func cleanTitle(title string) string {
	title = strings.TrimSpace(title)
	if i := strings.IndexAny(title, "\r\n"); i >= 0 {
		title = strings.TrimSpace(title[:i])
	}
	title = strings.Trim(title, `"'`)
	if utf8.RuneCountInString(title) <= maxConversationTitle {
		return title
	}
	runes := []rune(title)
	return string(runes[:maxConversationTitle])
}

func (a *Agent) saveTurn(ctx context.Context, sessionID uint, question, answer string) error {
	if a.memory == nil || sessionID == 0 {
		return nil
	}
	return a.memory.AppendMessages(ctx, sessionID, []store.Message{
		{Role: "user", Content: question},
		{Role: "assistant", Content: answer},
	})
}

// 预加载历史对话的总计清单，让模型不用每次根据全部历史重新生成，而是直接从清单里提取记忆。
// 下面几段还没写完，先留着，不参与编译。
//
// type MemoryBrief struct {
// 	ConversationID uint   `json:"conversation_id"`
// 	SessionID      []uint `json:"session_id"`
// 	Brief          string `json:"brief"`
// 	// raw ? 如果太多 ?
// }
//
// // 加载同一对话下的长期记忆
// func InjectPostMemory(ctx context.Context, conversationID uint) {
// 	// @todo 区分在同一轮对话下的不同 session 的记忆
// 	memoryBrief, err := GenerateMemoryBrief(ctx, conversationID)
// }
//
// func GenerateMemoryBrief(ctx context.Context, conversationID uint) (MemoryBrief, error) {
// 	// 根据 conversationID 找 session，找 memory，然后汇总
// 	// warn：可能很粗糙，准确性不高
// }
//
// // 应该在生成记忆的时候再让模型处理一轮，评估优先级和一些 tag（分类）
// // @todo 后期做 multi-agent 的时候，应该分出一个 executor 专门执行，参数由发任务的 agent 给的单子带上，会带上 user 期望的
