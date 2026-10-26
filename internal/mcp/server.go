package mcp

import (
	"context"
	"fmt"
	"strings"

	"recallweave/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type searchInput struct {
	Keyword string `json:"keyword" jsonschema:"Words or question to search in past memories"`
	Tag     string `json:"tag,omitempty" jsonschema:"Optional memory tag: work, study, tech, product, decision, people, life, or other"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum results, from 1 to 20; defaults to 5"`
}

type memoryHit struct {
	MemoryID    uint   `json:"memory_id"`
	SessionID   uint   `json:"session_id"`
	SeqStart    int    `json:"seq_start"`
	SeqEnd      int    `json:"seq_end"`
	Tag         string `json:"tag"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
}

type searchOutput struct {
	Memories []memoryHit `json:"memories"`
}

type getMessagesInput struct {
	SessionID uint `json:"session_id" jsonschema:"Session ID returned by search_memories"`
	SeqStart  int  `json:"seq_start" jsonschema:"First message number to retrieve"`
	SeqEnd    int  `json:"seq_end" jsonschema:"Last message number to retrieve, inclusive"`
}

type messageLine struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type getMessagesOutput struct {
	SessionID uint          `json:"session_id"`
	Messages  []messageLine `json:"messages"`
}

type memoryReader interface {
	FindMemories(context.Context, string, store.MemoryTag, int) ([]store.Memory, error)
	GetMessages(context.Context, uint, int, int) ([]store.Message, error)
}

type McpToolSet struct {
	tools []*mcp.Tool
}

func NewMcpServer(reader memoryReader) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "recallweave",
			Version: "0.1.0",
		},
		nil,
	)
	RegisteTools(server, reader)
	return server
}

func RegisteTools(server *mcp.Server, reader memoryReader) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_memories",
		Description: "Search RecallWeave's personal memories. Results include session and message numbers for checking original text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, searchOutput, error) {
		keyword := strings.TrimSpace(input.Keyword)
		if keyword == "" {
			return nil, searchOutput{}, fmt.Errorf("keyword is required")
		}
		if input.Limit < 0 || input.Limit > 20 {
			return nil, searchOutput{}, fmt.Errorf("limit must be between 1 and 20")
		}
		var tag store.MemoryTag
		if input.Tag != "" {
			for _, allowed := range store.MemoryTags {
				if input.Tag == string(allowed) {
					tag = allowed
					break
				}
			}
			if tag == "" {
				return nil, searchOutput{}, fmt.Errorf("unknown memory tag %q", input.Tag)
			}
		}
		items, err := reader.FindMemories(ctx, keyword, tag, input.Limit)
		if err != nil {
			return nil, searchOutput{}, err
		}
		out := searchOutput{Memories: make([]memoryHit, 0, len(items))}
		for _, item := range items {
			out.Memories = append(out.Memories, memoryHit{
				MemoryID: item.ID, SessionID: item.SessionID,
				SeqStart: item.SeqStart, SeqEnd: item.SeqEnd,
				Tag: string(item.Tag), Title: item.Title, Content: item.Content,
				ContentType: string(store.NormalizeContentType(string(item.ContentType))),
			})
		}
		return nil, out, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_messages",
		Description: "Read original messages by session ID and message-number range from search_memories. Use to verify a memory before citing it. Maximum 50 messages per call.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getMessagesInput) (*mcp.CallToolResult, getMessagesOutput, error) {
		if input.SessionID == 0 {
			return nil, getMessagesOutput{}, fmt.Errorf("session_id is required")
		}
		if input.SeqStart < 1 || input.SeqEnd < input.SeqStart {
			return nil, getMessagesOutput{}, fmt.Errorf("invalid message range")
		}
		if input.SeqEnd-input.SeqStart+1 > 50 {
			return nil, getMessagesOutput{}, fmt.Errorf("message range exceeds 50 messages")
		}
		messages, err := reader.GetMessages(ctx, input.SessionID, input.SeqStart, input.SeqEnd)
		if err != nil {
			return nil, getMessagesOutput{}, err
		}
		out := getMessagesOutput{SessionID: input.SessionID, Messages: make([]messageLine, 0, len(messages))}
		for _, msg := range messages {
			out.Messages = append(out.Messages, messageLine{Seq: msg.Seq, Role: msg.Role, Content: msg.Content})
		}
		return nil, out, nil
	})
}
