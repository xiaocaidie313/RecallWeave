package chat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/extract"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"
	"recallweave/internal/watch/useage"
)

var ErrNoModel = errors.New("chat: llm is not configured")

const defaultConversationTitle = "新对话"
const maxConversationTitle = 60
const keepRecentMessages = 20

const titlePrompt = `根据用户的问题给这段对话起一个简短标题。只返回标题本身，不要解释，不要加引号。`

const briefPrompt = `把对话压缩成一份可供后续继续聊天使用的摘要。保留决定、偏好、未完成的事和关键事实。只返回摘要本身，不要解释。`

// systemPrompt 是问答场景的通用规则。身份由 Agent 的 Role 给出，
// 记忆工具的用法由 memory ability 的 Instructions 给出，这里不再重复。
const systemPrompt = `普通问题直接回答，不要为了检索而检索。`

type Answer struct {
	Text      string     `json:"answer"`
	Citations []Citation `json:"citations"`
	Rounds    int        `json:"rounds"`
	RunID     string     `json:"run_id,omitempty"`
	Usage     Usage      `json:"usage"`
	Timings   Timings    `json:"timings"`
}

// Timings 记录请求关键路径；后台标题和提炼不计入 TotalMs。
type Timings struct {
	TotalMs       int64 `json:"total_ms"`
	LoadContextMs int64 `json:"load_context_ms"`
	AgentMs       int64 `json:"agent_ms"`
	SaveTurnMs    int64 `json:"save_turn_ms"`
	CondenseMs    int64 `json:"condense_ms"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	LatencyMs        int64 `json:"latency_ms"`
	Calls            int   `json:"calls"`
}

// Agent 是问答服务：准备上下文 → 交给 core 跑 → 保存、提炼、压缩、起标题。
// 真正决定调哪个工具的是 core（agent.Agent），这里的步骤每次都固定执行。
type Agent struct {
	core      *agent.Agent
	memory    *memory.MemoryManger
	extracter *extract.ExtractExcuter
	delegator runtime.Delegator
}

// core 或 core.Model 为 nil 表示没配 api_key，这时问答直接报错。
// 提炼有本地兜底，问答没有——没有模型就没法组织回答。
func NewAgent(core *agent.Agent, memoryManger *memory.MemoryManger, extracter *extract.ExtractExcuter) *Agent {
	return &Agent{core: core, memory: memoryManger, extracter: extracter}
}

// WithDelegator is configured once during assembly. Per-run delegation state
// stays inside runtime.Run, including for already-scoped branch Agents.
func (a *Agent) WithDelegator(delegator runtime.Delegator) *Agent {
	a.delegator = delegator
	return a
}

func (a *Agent) model() llm.ChatGenerator {
	if a.core == nil {
		return nil
	}
	return a.core.Model
}

func (a *Agent) Chat(ctx context.Context, question string, conversationID uint) (Answer, error) {
	requestStarted := time.Now()
	var timings Timings
	if a.model() == nil {
		return Answer{}, ErrNoModel
	}

	stageStarted := time.Now()
	preload, sessionID, conversation, visible, err := a.loadContext(ctx, conversationID, question)
	timings.LoadContextMs = time.Since(stageStarted).Milliseconds()
	if err != nil {
		return Answer{}, err
	}
	// 请求结束后上下文会被取消，标题生成改用不会跟着取消的上下文，并写回数据库。
	go a.generateTitle(context.WithoutCancel(ctx), conversationID, question)
	core := a.core
	if conversation != nil && conversation.IsBranch() {
		core = branchCore(a.core, visible)
	}

	stageStarted = time.Now()
	result, err := runtime.Run(ctx, core, runtime.Request{
		Input:          question,
		ConversationID: conversationID,
		SystemPrompt:   systemPrompt,
		Preload:        preload,
		Delegator:      a.delegator,
	})
	timings.AgentMs = time.Since(stageStarted).Milliseconds()
	logToolErrors(result.State)
	a.saveRun(context.WithoutCancel(ctx), result)
	if err != nil {
		return Answer{}, err
	}

	stageStarted = time.Now()
	if err := a.saveTurn(ctx, sessionID, question, result.Content); err != nil {
		return Answer{}, err
	}
	timings.SaveTurnMs = time.Since(stageStarted).Milliseconds()
	if conversation == nil || !conversation.IsBranch() {
		go a.extractSession(context.WithoutCancel(ctx), sessionID)
	}
	stageStarted = time.Now()
	a.condenseWindow(ctx, conversationID, sessionID)
	timings.CondenseMs = time.Since(stageStarted).Milliseconds()
	timings.TotalMs = time.Since(requestStarted).Milliseconds()
	slog.Info("chat timing", "run_id", result.State.RunID, "total_ms", timings.TotalMs,
		"load_context_ms", timings.LoadContextMs, "agent_ms", timings.AgentMs,
		"save_turn_ms", timings.SaveTurnMs, "condense_ms", timings.CondenseMs)

	answer := answerFromResult(result)
	answer.Timings = timings
	return answer, nil
}

func answerFromResult(result runtime.RunResult) Answer {
	calls := result.State.AllCalls()
	total := useage.Sum(calls)
	return Answer{
		Text:      result.Content,
		Citations: citationsFromSteps(result.State.AllSteps()),
		Rounds:    result.State.CurrentRound,
		RunID:     result.State.RunID,
		Usage: Usage{
			PromptTokens:     total.Usage.PromptTokens,
			CompletionTokens: total.Usage.CompletionTokens,
			ReasoningTokens:  total.Usage.ReasoningTokens,
			CachedTokens:     total.Usage.CachedTokens,
			LatencyMs:        total.LatencyMs,
			Calls:            len(calls),
		},
	}
}

func (a *Agent) saveRun(ctx context.Context, result runtime.RunResult) {
	if a.memory == nil || result.State.RunID == "" {
		return
	}
	for _, child := range result.State.Children {
		a.saveRun(ctx, child)
	}
	encoded, err := json.Marshal(result.State)
	if err != nil {
		slog.Warn("encode run state failed", "run_id", result.State.RunID, "error", err)
		return
	}
	total := useage.Sum(result.State.AllCalls())
	if err := a.memory.SaveAgentRun(ctx, store.AgentRun{
		RunID:            result.State.RunID,
		ParentRunID:      result.State.ParentRunID,
		ConversationID:   result.State.ConversationID,
		Status:           string(result.State.Status),
		Input:            result.State.Input,
		Content:          result.Content,
		State:            encoded,
		PromptTokens:     total.Usage.PromptTokens,
		CompletionTokens: total.Usage.CompletionTokens,
		LatencyMs:        total.LatencyMs,
		Rounds:           result.State.CurrentRound,
	}); err != nil {
		slog.Warn("save run failed", "run_id", result.State.RunID, "error", err)
	}
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
