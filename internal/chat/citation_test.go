package chat

import (
	"encoding/json"
	"testing"

	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
)

func TestCitationsFromStepsRequiresGetMessages(t *testing.T) {
	hits := ability.SearchMemoriesResult{Memories: []ability.MemoryHit{
		{MemoryID: 4, SessionID: 1, SeqStart: 1, SeqEnd: 2, Title: "cmd 目录"},
		{MemoryID: 9, SessionID: 2, SeqStart: 3, SeqEnd: 4, Title: "另一条"},
	}}
	encoded, err := json.Marshal(hits)
	if err != nil {
		t.Fatalf("marshal hits: %v", err)
	}

	if citations := citationsFromSteps([]runtime.RunStep{
		{ToolName: ability.SearchMemoriesToolName, Result: string(encoded)},
	}); len(citations) != 0 {
		t.Fatalf("search-only hits should not become citations, got %+v", citations)
	}

	citations := citationsFromSteps([]runtime.RunStep{
		{ToolName: ability.SearchMemoriesToolName, Result: string(encoded)},
		{ToolName: ability.SearchMemoriesToolName, Result: string(encoded)},
		{ToolName: ability.SearchMemoriesToolName, Error: "db is down", Result: string(encoded)},
		{
			ToolName:  ability.GetMessagesToolName,
			Arguments: `{"session_id":1,"seq_start":1,"seq_end":2}`,
			Result:    `{"session_id":1,"messages":[{"seq":1,"role":"user","content":"cmd 目录是入口"},{"seq":2,"role":"assistant","content":"明白"}]}`,
		},
		{ToolName: ability.SearchMemoriesToolName, Result: `{`},
	})

	if len(citations) != 1 {
		t.Fatalf("expected 1 verified citation, got %d: %+v", len(citations), citations)
	}
	if citations[0].MemoryID != 4 || citations[0].SessionID != 1 {
		t.Fatalf("unexpected citations: %+v", citations)
	}
	if citations[0].SeqStart != 1 || citations[0].SeqEnd != 2 {
		t.Fatalf("citation should point to returned messages: %+v", citations)
	}
}

func TestCitationsRequireReturnedMessages(t *testing.T) {
	search := runtime.RunStep{ToolName: ability.SearchMemoriesToolName,
		Result: `{"memories":[{"memory_id":4,"session_id":1,"seq_start":1,"seq_end":5,"title":"决定"}]}`}
	for _, result := range []string{`{"session_id":1,"messages":[]}`, `{"session_id":1}`, `{`} {
		steps := []runtime.RunStep{search, {ToolName: ability.GetMessagesToolName,
			Arguments: `{"session_id":1,"seq_start":1,"seq_end":5}`, Result: result}}
		if got := citationsFromSteps(steps); len(got) != 0 {
			t.Fatalf("empty or invalid result %q produced citations: %+v", result, got)
		}
	}
	steps := []runtime.RunStep{search, {ToolName: ability.GetMessagesToolName,
		Result: `{"session_id":1,"messages":[{"seq":2,"content":"原文"},{"seq":4,"content":"另一条原文"}]}`}}
	got := citationsFromSteps(steps)
	if len(got) != 2 || got[0].SeqStart != 2 || got[0].SeqEnd != 2 || got[1].SeqStart != 4 || got[1].SeqEnd != 4 {
		t.Fatalf("citations should only include returned messages: %+v", got)
	}
	steps[1].Error = "db is down"
	if got := citationsFromSteps(steps); len(got) != 0 {
		t.Fatalf("failed tool produced citations: %+v", got)
	}
}
