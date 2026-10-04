package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"recallweave/internal/extract"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"
)

// maxRounds 限制「模型调工具 → 回灌结果」的往返次数。没有上限的话，
// 模型可能一直要工具，把一次请求拖成无限循环。
const maxRounds = 3

var ErrNoModel = errors.New("ask: llm is not configured")

const defaultConversationTitle = "新对话"
const maxConversationTitle = 60
const keepRecentMessages = 20
// preloadedMemoriesLimit 混合检索预取的记忆条数。只放几条摘要进上下文，
// 控制住 token 成本，细节让模型用工具自己挖。
const preloadedMemoriesLimit = 4

const titlePrompt = `根据用户的问题给这段对话起一个简短标题。只返回标题本身，不要解释，不要加引号。`

const briefPrompt = `把对话压缩成一份可供后续继续聊天使用的摘要。保留决定、偏好、未完成的事和关键事实。只返回摘要本身，不要解释。`

// systemPrompt 区分两类问题。
// 普通问题直接回答。涉及用户过去的对话时，优先用预载的记忆摘要；
// 摘要不够用或要核对原文，才用工具深挖。找不到记录就说明没有，再按常识答。
const systemPrompt = `你是 RecallWeave 的助手。

普通问题直接回答，不要为了检索而检索。

系统消息里可能已经预载了与问题相关的记忆摘要，涉及用户过去时先直接引用它们。
当摘要不够用、或需要核实原文时：
- 用 search_memories 检索更多记忆，必要时再用 get_messages 核对原文
- 记忆里找不到依据时，先说明没有相关记录，再按常识继续回答
- 用到记忆时，点明依据来自哪个会话的哪几条消息`

// Citation 是一条回答依据，指回某条记忆和它在原文里的位置。
type Citation struct {
	MemoryID  uint   `json:"memory_id"`
	SessionID uint   `json:"session_id"`
	SeqStart  int    `json:"seq_start"`
	SeqEnd    int    `json:"seq_end"`
	Title     string `json:"title"`
}

type Answer struct {
	Text      string     `json:"answer"`
	Citations []Citation `json:"citations"`
	Rounds    int        `json:"rounds"`
}

type Agent struct {
	responder llm.Responder
	tools     *llm.ToolHandle
	memory    *memory.MemoryManger
	extracter *extract.ExtractExcuter
}

// responder 为 nil 表示没配 api_key，这时问答直接报错。
// 提炼有本地兜底，问答没有——没有模型就没法组织回答。
func NewAgent(responder llm.Responder, tools *llm.ToolHandle, memoryManger *memory.MemoryManger, extracter *extract.ExtractExcuter) *Agent {
	return &Agent{responder: responder, tools: tools, memory: memoryManger, extracter: extracter}
}

func (a *Agent) Ask(ctx context.Context, question string, conversationID uint) (Answer, error) {
	if a.responder == nil {
		return Answer{}, ErrNoModel
	}

	conversation := llm.NewConversation(systemPrompt, conversationID)
	// 预载上下文：历史对话 + 简报 + 与问题相关的记忆（混合检索）
	sessionID, err := a.preloadContext(ctx, conversation, conversationID, question)
	if err != nil {
		return Answer{}, err
	}
	conversation.AddUserMessage(question)
	// 请求结束后上下文会被取消，标题生成改用不会跟着取消的上下文，并写回数据库。
	go a.generateTitle(context.WithoutCancel(ctx), conversationID, question)

	var citations []Citation
	// 模型往往会换关键词多搜几轮，同一条记忆会重复命中，这里按 id 去重。
	cited := make(map[uint]struct{})

	for round := 1; round <= maxRounds; round++ {
		turn, err := a.responder.Next(ctx, conversation, a.tools.Schemas())
		if err != nil {
			return Answer{}, err
		}

		// 没有工具调用，直接返回回答 1 进行的对话不需要工具  2 这一轮对话 工具用完了
		if len(turn.ToolCalls) == 0 {
			if err := a.saveTurn(ctx, sessionID, question, turn.Content); err != nil {
				return Answer{}, err
			}
			a.extractSession(ctx, sessionID)
			a.condenseWindow(ctx, conversationID, sessionID)
			return Answer{
				Text:      turn.Content,
				Citations: citations,
				Rounds:    round,
			}, nil
		}

		// 有工具调用，执行工具
		for _, call := range turn.ToolCalls {
			result, err := a.tools.Run(ctx, call.Name, call.Arguments)
			if err != nil {
				// 工具失败不该终止整轮问答，把原因回灌给模型，让它换个问法或者换个工具。
				slog.Warn("tool call failed",
					"tool", call.Name,
					"arguments", call.Arguments,
					"error", err,
				)
				conversation.AddToolResult(call.ID, toolError(err))
				continue
			}
			// 根据工具名称，解析结果，生成引用  专用引用
			for _, citation := range citationsFrom(call.Name, result) {
				if _, seen := cited[citation.MemoryID]; seen {
					continue
				}
				cited[citation.MemoryID] = struct{}{} // 去重  标记为已访问
				citations = append(citations, citation)
			}
			// 添加工具调用结果
			conversation.AddToolResult(call.ID, result)
		}
	}
	return Answer{}, fmt.Errorf("ask: gave up after %d rounds of tool calls", maxRounds)
}

func toolError(err error) string {
	encoded, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return `{"error":"tool failed"}`
	}
	return string(encoded)
}

// @todo 做个map 或者总的 switch case
// citationsFrom 从检索结果里收集来源。工具是启动时注册一次、请求间共享的，
// 没法在里面存单次请求的状态，所以由 agent 解析结果来攒引用。
func citationsFrom(toolName, result string) []Citation {

	switch toolName {
	case "search_memories":
		var parsed searchMemoriesResult
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			return nil
		}

		citations := make([]Citation, 0, len(parsed.Memories))
		for _, hit := range parsed.Memories {
			citations = append(citations, Citation{
				MemoryID:  hit.MemoryID,
				SessionID: hit.SessionID,
				SeqStart:  hit.SeqStart,
				SeqEnd:    hit.SeqEnd,
				Title:     hit.Title,
			})
		}
		return citations
	default:
		return []Citation{}
	}
}

// preloadContext 把这次提问需要的全部背景放进上下文，并返回对应的 session。
// 分两个层面：
//  1. 记忆层：preloadMemories 从全库检索与问题相关的记忆（混合检索），
//     模型开答前就知道用户过去的相关经历，不必先学会调工具。
//  2. 对话层：本对话的历史消息 + 之前的简报，模型才知道「这次聊到哪了」。
// 记忆层和对话层是两套数据：记忆来自所有导入来源的提炼（微信、GPT、Cursor…），
// 对话是本 conversation 专属的 chat session。别混在一起看。
// 没有记忆库或没有 conversation 时跳过，测试可以不连数据库。
func (a *Agent) preloadContext(ctx context.Context, conv *llm.Conversation, conversationID uint, question string) (uint, error) {
	if a.memory == nil || conversationID == 0 {
		return 0, nil
	}
	if question != "" {
		a.preloadMemories(ctx, conv, question)
	}

	session, err := a.memory.ChatSession(ctx, conversationID)
	if err != nil {
		return 0, err
	}

	brief, untilSeq, err := a.memory.ConversationBrief(ctx, conversationID)
	if err != nil {
		return 0, err
	}
	if brief != "" {
		conv.AddUserMessage("此前对话摘要：\n" + brief)
	}

	messages, err := a.memory.ListMessages(ctx, session.ID)
	if err != nil {
		return 0, err
	}
	for _, msg := range messages {
		if untilSeq > 0 && msg.Seq <= untilSeq {
			continue
		}
		if msg.Role == "assistant" {
			conv.AddAssistantMessage(msg.Content)
			continue
		}
		conv.AddUserMessage(msg.Content)
	}
	return session.ID, nil
}

// preloadMemories 用当前问题从记忆库检索相关记忆，把 top-K 的标题+摘要拼成
// 一条 system 消息注入。这是「记忆层」的预载：搜的是全库提炼过的 memory，
// 不是本对话的 session。
// 混合检索：让模型在不开工具的情况下也能答对「记得的事」。
// 这些预取的记忆不进 citations——它们是「可能的背景」，真正的引用依据走工具链。
// 检索失败只记日志，不让预取失败拦截掉这一次提问。
func (a *Agent) preloadMemories(ctx context.Context, conv *llm.Conversation, question string) {
	hits, err := a.memory.SearchMemories(ctx, question, "", preloadedMemoriesLimit)
	if err != nil {
		slog.Warn("preload memories failed", "error", err)
		return
	}
	if len(hits) == 0 {
		return
	}

	var builder strings.Builder
	builder.WriteString("用户过去可能相关的经历：\n")
	for _, hit := range hits {
		builder.WriteString("- ")
		builder.WriteString(hit.Title)
		builder.WriteString("\n  ")
		builder.WriteString(hit.Content)
		builder.WriteByte('\n')
	}
	builder.WriteString("\n这些仅供参考，回答时如果你不确定，用 search_memories 或 get_messages 核实。")

	conv.AddSystemMessage(builder.String())
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
	if a.memory == nil || a.responder == nil || conversationID == 0 || sessionID == 0 {
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
	next, err := a.responder.NewOneTurnChat(ctx, briefPrompt, briefInput(brief, fresh))
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
	if a.memory == nil || a.responder == nil || conversationID == 0 {
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

	title, err := a.responder.NewOneTurnChat(ctx, titlePrompt, question)
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
