package main

import (
	"context"
	"testing"

	appmcp "recallweave/internal/mcp"
	"recallweave/internal/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeFinder struct {
	keyword   string
	limit     int
	sessionID uint
	seqStart  int
	seqEnd    int
	messages  []store.Message
}

func (f *fakeFinder) FindMemories(_ context.Context, keyword string, _ store.MemoryTag, limit int) ([]store.Memory, error) {
	f.keyword, f.limit = keyword, limit
	return []store.Memory{{ID: 7, SessionID: 3, SeqStart: 4, SeqEnd: 5, Title: "数据库决定", Content: "使用 PostgreSQL", ContentType: store.ContentDecision}}, nil
}

func (f *fakeFinder) GetMessages(_ context.Context, sessionID uint, seqStart, seqEnd int) ([]store.Message, error) {
	f.sessionID, f.seqStart, f.seqEnd = sessionID, seqStart, seqEnd
	return f.messages, nil
}

func TestSearchMemoriesOverMCP(t *testing.T) {
	ctx := context.Background()
	finder := &fakeFinder{}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := appmcp.NewMcpServer(finder).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != 2 || listed.Tools[0].Name != "get_messages" || listed.Tools[1].Name != "search_memories" {
		t.Fatalf("unexpected MCP tools: %+v", listed.Tools)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "search_memories", Arguments: map[string]any{"keyword": "PostgreSQL", "limit": 2},
	})
	if err != nil || result.IsError {
		t.Fatalf("call search_memories: result=%+v err=%v", result, err)
	}
	if finder.keyword != "PostgreSQL" || finder.limit != 2 {
		t.Fatalf("unexpected search arguments: keyword=%q limit=%d", finder.keyword, finder.limit)
	}
	content, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("unexpected structured content: %T", result.StructuredContent)
	}
	items, ok := content["memories"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected memories: %+v", content["memories"])
	}
	hit, ok := items[0].(map[string]any)
	if !ok || hit["session_id"] != float64(3) || hit["title"] != "数据库决定" {
		t.Fatalf("unexpected memory: %+v", items[0])
	}
	if hit["content_type"] != "decision" {
		t.Fatalf("content type missing from MCP output: %+v", hit)
	}
	if _, leaked := hit["embedding"]; leaked {
		t.Fatal("embedding must not be exposed by MCP")
	}
}

func TestGetMessagesOverMCP(t *testing.T) {
	ctx := context.Background()
	finder := &fakeFinder{messages: []store.Message{{SessionID: 3, Seq: 4, Role: "user", Content: "使用 PostgreSQL"}}}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := appmcp.NewMcpServer(finder).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_messages", Arguments: map[string]any{"session_id": 3, "seq_start": 4, "seq_end": 5},
	})
	if err != nil || result.IsError {
		t.Fatalf("call get_messages: result=%+v err=%v", result, err)
	}
	if finder.sessionID != 3 || finder.seqStart != 4 || finder.seqEnd != 5 {
		t.Fatalf("unexpected message range: session=%d start=%d end=%d", finder.sessionID, finder.seqStart, finder.seqEnd)
	}
	content, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("unexpected structured content: %T", result.StructuredContent)
	}
	items, ok := content["messages"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected messages: %+v", content["messages"])
	}
	message := items[0].(map[string]any)
	if message["seq"] != float64(4) || message["content"] != "使用 PostgreSQL" {
		t.Fatalf("unexpected message: %+v", message)
	}
	if _, leaked := message["source_tag"]; leaked {
		t.Fatal("message metadata must not be exposed by MCP")
	}

	for _, args := range []map[string]any{
		{"session_id": 0, "seq_start": 1, "seq_end": 2},
		{"session_id": 3, "seq_start": 0, "seq_end": 2},
		{"session_id": 3, "seq_start": 5, "seq_end": 4},
		{"session_id": 3, "seq_start": 1, "seq_end": 51},
	} {
		invalid, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_messages", Arguments: args})
		if err != nil || !invalid.IsError {
			t.Fatalf("invalid range should return tool error: args=%v result=%+v err=%v", args, invalid, err)
		}
	}

	finder.messages = nil
	empty, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_messages", Arguments: map[string]any{"session_id": 3, "seq_start": 9, "seq_end": 10},
	})
	if err != nil || empty.IsError {
		t.Fatalf("empty range should succeed: result=%+v err=%v", empty, err)
	}
	emptyContent := empty.StructuredContent.(map[string]any)
	if messages, ok := emptyContent["messages"].([]any); !ok || len(messages) != 0 {
		t.Fatalf("expected empty messages array, got %+v", emptyContent["messages"])
	}
}
