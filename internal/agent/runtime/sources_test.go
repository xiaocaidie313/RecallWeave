package runtime

import (
	"reflect"
	"testing"
)

func TestDelegationSourcesUseReturnedMessagesOnly(t *testing.T) {
	steps := []RunStep{
		{ToolName: "search_memories", Result: `{"memories":[{"session_id":8,"seq_start":1,"seq_end":20}]}`},
		{ToolName: "get_messages", Arguments: `{"session_id":8,"seq_start":1,"seq_end":20}`,
			Result: `{"session_id":8,"messages":[{"seq":4,"content":"four"},{"seq":2,"content":"two"},{"seq":3,"content":"three"},{"seq":4,"content":"duplicate"},{"seq":8,"content":"eight"},{"seq":9,"content":""},{"seq":0,"content":"invalid"}]}`},
		{ToolName: "get_messages", Result: `{"session_id":8,"messages":[]}`},
		{ToolName: "get_messages", Result: `{"session_id":9,"messages":[{"seq":1,"content":"failed"}]}`, Error: "unavailable"},
		{ToolName: "get_messages", Result: `{"session_id":0,"messages":[{"seq":1,"content":"invalid"}]}`},
		{ToolName: "get_messages", Result: `{`},
	}
	want := []Source{
		{Kind: "message", Tool: "get_messages", SessionID: 8, SeqStart: 2, SeqEnd: 4},
		{Kind: "message", Tool: "get_messages", SessionID: 8, SeqStart: 8, SeqEnd: 8},
	}
	if got := SourcesFromSteps(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("sources must match actual returned ranges: %+v", got)
	}
	if got := SourcesFromSteps(steps[:1]); len(got) != 0 {
		t.Fatalf("memory summaries are not original-message evidence: %+v", got)
	}
}

func TestDelegationSourcesUseKnownWebToolResults(t *testing.T) {
	steps := []RunStep{
		{ToolName: "mcp_tavily_search", Result: `{"results":[{"url":"https://example.test/docs","title":"Docs","content":"snippet"},{"url":"javascript:alert(1)","content":"bad"},{"url":"https://user:secret@example.test/","content":"bad"},{"url":"https://example.test/empty"}]}`},
		{ToolName: "mcp_tavily_extract", Result: `{"results":[{"url":"https://example.test/page","raw_content":"page body"}]}`},
		{ToolName: "arbitrary_mcp", Result: `{"results":[{"url":"https://example.test/untrusted","content":"body"}]}`},
		{ToolName: "mcp_tavily_search", Result: `{"results":[{"url":"https://example.test/failed","content":"body"}]}`, Error: "failure"},
		{ToolName: "mcp_tavily_search", Result: "not structured JSON"},
	}
	want := []Source{
		{Kind: "web", Tool: "mcp_tavily_search", URL: "https://example.test/docs", Title: "Docs"},
		{Kind: "web", Tool: "mcp_tavily_extract", URL: "https://example.test/page"},
	}
	if got := SourcesFromSteps(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected web sources: %+v", got)
	}
	if got := SourcesFromSteps(append(steps, steps[0])); !reflect.DeepEqual(got, want) {
		t.Fatalf("duplicate tool result duplicated sources: %+v", got)
	}
}
