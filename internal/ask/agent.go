package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"recallweave/internal/llm"
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
}

// responder 为 nil 表示没配 api_key，这时问答直接报错。
// 提炼有本地兜底，问答没有——没有模型就没法组织回答。
func NewAgent(responder llm.Responder, tools *llm.ToolSet) *Agent {
	return &Agent{responder: responder, tools: tools}
}

func (a *Agent) Ask(ctx context.Context, question string) (Answer, error) {
	if a.responder == nil {
		return Answer{}, ErrNoModel
	}

	conversation := llm.NewConversation(systemPrompt)
	conversation.AddUser(question)

	var citations []Citation
	// 模型往往会换关键词多搜几轮，同一条记忆会重复命中，这里按 id 去重。
	cited := make(map[uint]struct{})

	for round := 1; round <= maxRounds; round++ {
		turn, err := a.responder.Next(ctx, conversation, a.tools.Schemas())
		if err != nil {
			return Answer{}, err
		}

		if len(turn.ToolCalls) == 0 {
			return Answer{
				Text:      turn.Content,
				Citations: citations,
				Rounds:    round,
			}, nil
		}

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
				cited[citation.MemoryID] = struct{}{}
				citations = append(citations, citation)
			}

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
