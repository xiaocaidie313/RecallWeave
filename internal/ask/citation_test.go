package ask

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
			Result:    `{"session_id":1}`,
		},
		{ToolName: ability.SearchMemoriesToolName, Result: `{`},
	})

	if len(citations) != 1 {
		t.Fatalf("expected 1 verified citation, got %d: %+v", len(citations), citations)
	}
	if citations[0].MemoryID != 4 || citations[0].SessionID != 1 {
		t.Fatalf("unexpected citations: %+v", citations)
	}
}
