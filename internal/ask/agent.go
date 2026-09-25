package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"
)

// maxRounds 限制「模型调工具 → 回灌结果」的往返次数。没有上限的话，
// 模型可能一直要工具，把一次请求拖成无限循环。
const maxRounds = 3

var ErrNoModel = errors.New("ask: llm is not configured")

const systemPrompt = `你是 RecallWeave 的记忆助手，回答依据只能是用户过去的记忆。

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
}

// responder 为 nil 表示没配 api_key，这时问答直接报错。
// 提炼有本地兜底，问答没有——没有模型就没法组织回答。
func NewAgent(responder llm.Responder, tools *llm.ToolHandle, memoryManger *memory.MemoryManger) *Agent {
	return &Agent{responder: responder, tools: tools, memory: memoryManger}
}

func (a *Agent) Ask(ctx context.Context, question string, conversationID uint) (Answer, error) {
	if a.responder == nil {
		return Answer{}, ErrNoModel
	}

	conversation := llm.NewConversation(systemPrompt)
	sessionID, err := a.preload(ctx, conversation, conversationID)
	if err != nil {
		return Answer{}, err
	}
	conversation.AddUserMessage(question)

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

// citationsFrom 从检索结果里收集来源。工具是启动时注册一次、请求间共享的，
// 没法在里面存单次请求的状态，所以由 agent 解析结果来攒引用。
func citationsFrom(toolName, result string) []Citation {
	if toolName != "search_memories" {
		return nil
	}

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
}

// preload 把这次聊天里已经有的消息放进上下文，并返回对应的 session。
// 没有记忆库或没有 conversation 时跳过，测试可以不连数据库。
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
