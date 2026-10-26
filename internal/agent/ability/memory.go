package ability

import (
	"context"
	"encoding/json"
	"fmt"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"
)

const SearchMemoriesToolName = "search_memories"
const GetMessagesToolName = "get_messages"

const memoryInstructions = `涉及用户过去的经历时，先引用系统消息里预载的记忆摘要。
摘要不够用或需要核实原文时：
- 用 search_memories 检索更多记忆，必要时再用 get_messages 核对原文
- 记忆里找不到依据时，先说明没有相关记录，再按常识继续回答
- 用到记忆时，点明依据来自哪个会话的哪几条消息`

// MemoryHit 是回灌给模型的一条记忆。带上 session_id 和 seq 区间，
// 模型才能引用来源，也才能接着调 get_messages 看原文。  命中的记忆
type MemoryHit struct {
	MemoryID    uint   `json:"memory_id"`
	SessionID   uint   `json:"session_id"`
	SeqStart    int    `json:"seq_start"`
	SeqEnd      int    `json:"seq_end"`
	Tag         string `json:"tag"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
}

type SearchMemoriesResult struct {
	Memories []MemoryHit `json:"memories"`
}

type messageLine struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type getMessagesResult struct {
	SessionID uint          `json:"session_id"`
	Messages  []messageLine `json:"messages"`
}

// NewMemoryAbility 只放只读、细粒度的数据访问。提炼那种流水线不放进来——
// 它的触发时机是确定的，不需要模型判断。
func NewMemoryAbility(manager *memory.MemoryManger) *agent.Ability {
	return agent.NewAbility(
		"memory",
		"检索用户过去的记忆，并核对原文消息",
		memoryInstructions,
		agent.ToolBox{
			searchMemoriesTool(manager),
			getMessagesTool(manager),
			getSessionsTool(manager),
		},
	)
}

func searchMemoriesTool(manager *memory.MemoryManger) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        SearchMemoriesToolName,
			Description: "按问题或关键词检索用户过去的记忆，返回标题、摘要和它在原文里的位置。意思相近也会命中。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"keyword": map[string]any{
						"type":        "string",
						"description": "检索词，可以是整句问题，也可以是几个关键词。留空表示不按文字限制。",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "主题标签，留空表示不限。",
						"enum":        allowedTags(),
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "最多返回几条，默认 5，上限 20。",
					},
				},
			},
		},
		Run: func(ctx context.Context, arguments string) (string, error) {
			var args struct {
				Keyword string `json:"keyword"`
				Tag     string `json:"tag"`
				Limit   int    `json:"limit"`
			}
			if err := decodeArguments(arguments, &args); err != nil {
				return "", err
			}

			var tag store.MemoryTag
			if args.Tag != "" {
				tag = store.NormalizeMemoryTag(args.Tag)
			}

			memories, err := manager.SearchMemories(ctx, args.Keyword, tag, args.Limit)
			if err != nil {
				return "", err
			}

			result := SearchMemoriesResult{Memories: make([]MemoryHit, 0, len(memories))}
			for _, item := range memories {
				result.Memories = append(result.Memories, MemoryHit{
					MemoryID:    item.ID,
					SessionID:   item.SessionID,
					SeqStart:    item.SeqStart,
					SeqEnd:      item.SeqEnd,
					Tag:         string(item.Tag),
					Title:       item.Title,
					Content:     item.Content,
					ContentType: string(store.NormalizeContentType(string(item.ContentType))),
				})
			}

			return ResultToString(result)
		},
	}
}

func getMessagesTool(manager *memory.MemoryManger) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        GetMessagesToolName,
			Description: "按会话和 seq 区间取出原始消息，用来核对某条记忆的依据。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "integer",
						"description": "会话 ID，来自 search_memories 的结果。",
					},
					"seq_start": map[string]any{
						"type":        "integer",
						"description": "起始消息编号。",
					},
					"seq_end": map[string]any{
						"type":        "integer",
						"description": "结束消息编号。",
					},
				},
				"required": []string{"session_id", "seq_start", "seq_end"},
			},
		},
		Run: func(ctx context.Context, arguments string) (string, error) {
			var args struct {
				SessionID uint `json:"session_id"`
				SeqStart  int  `json:"seq_start"`
				SeqEnd    int  `json:"seq_end"`
			}
			if err := decodeArguments(arguments, &args); err != nil {
				return "", err
			}

			if args.SessionID == 0 {
				return "", fmt.Errorf("session_id is required")
			}
			if args.SeqStart > args.SeqEnd {
				return "", fmt.Errorf("seq_start must not be greater than seq_end")
			}

			messages, err := manager.GetMessages(ctx, args.SessionID, args.SeqStart, args.SeqEnd)
			if err != nil {
				return "", err
			}

			result := getMessagesResult{
				SessionID: args.SessionID,
				Messages:  make([]messageLine, 0, len(messages)),
			}
			for _, msg := range messages {
				result.Messages = append(result.Messages, messageLine{
					Seq:     msg.Seq,
					Role:    msg.Role,
					Content: msg.Content,
				})
			}

			return ResultToString(result)
		},
	}
}

func getSessionsTool(manager *memory.MemoryManger) llm.Tool {
	return llm.Tool{
		Schema: llm.ToolSchema{
			Name:        "get_sessions",
			Description: "根据conversationID 获取相关的session对话",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"conversation_id": map[string]any{
						"type":        "integer",
						"description": "conversationID",
					},
				},
			},
		},
		Run: func(ctx context.Context, arguments string) (string, error) {
			var args struct {
				ConversationID uint `json:"conversation_id"`
			}
			if err := decodeArguments(arguments, &args); err != nil {
				return "", err
			}

			if args.ConversationID == 0 {
				return "", fmt.Errorf("conversation_id is required")
			}

			sessions, err := manager.GetSessions(ctx, args.ConversationID)
			if err != nil {
				return "", err
			}
			return ResultToString(sessions)
		},
	}
}

// decodeArguments 容忍空参数，模型在无参调用时会给空字符串。
func decodeArguments(arguments string, target any) error {
	if arguments == "" || arguments == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(arguments), target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func ResultToString(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func allowedTags() []string {
	tags := make([]string, 0, len(store.MemoryTags))
	for _, tag := range store.MemoryTags {
		tags = append(tags, string(tag))
	}
	return tags
}
