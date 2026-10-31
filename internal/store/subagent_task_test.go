package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSubagentTaskJSONDoesNotExposeFullExecutionContext(t *testing.T) {
	raw, err := json.Marshal(SubagentTask{ID: "task-1", SubagentID: "child-1", ConversationID: 7, Status: "completed", Result: json.RawMessage(`{"state":{"context_snapshot":"PRIVATE_HISTORY"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_HISTORY") || strings.Contains(string(raw), `"result"`) {
		t.Fatalf("full execution leaked: %s", raw)
	}
	for _, want := range []string{`"task_id":"task-1"`, `"subagent_id":"child-1"`, `"conversation_id":7`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing metadata %s: %s", want, raw)
		}
	}
}
