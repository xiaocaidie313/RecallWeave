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

const titlePrompt = `根据用户的问题给这段对话起一个简短标题。只返回标题本身，不要解释，不要加引号。`

const systemPrompt = `当用户没有涉及对过去记忆的提问时候，你正常回答问题。当用户的问题涉及过去的记忆或者直接告诉你结合过去的记忆
那么 你是 RecallWeave 的记忆助手，回答依据只能是用户过去的记忆。

工作方式：
- 先用 search_memories 检索相关记忆，必要时再用 get_messages 核对原文
- 记忆里找不到依据时，直接说没有相关记录，不要凭常识编造
- 回答里要点明依据来自哪个会话的哪几条消息`

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
	// 预加载历史对话
	sessionID, err := a.preload(ctx, conversation, conversationID)
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

// preload 把这次聊天里已经有的消息放进上下文，并返回对应的 session。
// 没有记忆库或没有 conversation 时跳过，测试可以不连数据库。
// 单纯的加入信息
func (a *Agent) preload(ctx context.Context, conv *llm.Conversation, conversationID uint) (uint, error) {
	if a.memory == nil || conversationID == 0 {
		return 0, nil
	}

	session, err := a.memory.ChatSession(ctx, conversationID)
	if err != nil {
		return 0, err
	}

	messages, err := a.memory.ListMessages(ctx, session.ID)
	if err != nil {
		return 0, err
	}
	for _, msg := range messages {
		if msg.Role == "assistant" {
			conv.AddAssistantMessage(msg.Content)
			continue
		}
		conv.AddUserMessage(msg.Content)
	}
	return session.ID, nil
}

// extractSession 在这一问已经写入会话之后，把整段聊天重新切成记忆。
// 没配提炼器、或这次没有会话时跳过。失败只记日志，不把已经生成的回答变成 500。
func (a *Agent) extractSession(ctx context.Context, sessionID uint) {
	if a.extracter == nil || sessionID == 0 {
		return
	}
	if _, err := a.extracter.ExtractSession(ctx, sessionID); err != nil {
		slog.Warn("extract session failed",
			"session_id", sessionID,
			"error", err,
		)
	}
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
