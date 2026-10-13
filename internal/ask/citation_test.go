package ask

import (
	"encoding/json"
	"testing"

	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
)

func TestCitationsFromStepsDeduplicatesAndSkipsErrors(t *testing.T) {
	hits := ability.SearchMemoriesResult{Memories: []ability.MemoryHit{
		{MemoryID: 4, SessionID: 1, SeqStart: 1, SeqEnd: 2, Title: "cmd 目录"},
		{MemoryID: 9, SessionID: 2, SeqStart: 3, SeqEnd: 4, Title: "另一条"},
	}}
	encoded, err := json.Marshal(hits)
	if err != nil {
		t.Fatalf("marshal hits: %v", err)
	}

	citations := citationsFromSteps([]runtime.RunStep{
		{ToolName: ability.SearchMemoriesToolName, Result: string(encoded)},
		{ToolName: ability.SearchMemoriesToolName, Result: string(encoded)},
		{ToolName: ability.SearchMemoriesToolName, Error: "db is down", Result: string(encoded)},
		{ToolName: "get_messages", Result: `{"session_id":1}`},
		{ToolName: ability.SearchMemoriesToolName, Result: `{`},
	})

	if len(citations) != 2 {
		t.Fatalf("expected 2 citations, got %d: %+v", len(citations), citations)
	}
	if citations[0].MemoryID != 4 || citations[1].MemoryID != 9 {
		t.Fatalf("unexpected citations: %+v", citations)
	}
}
