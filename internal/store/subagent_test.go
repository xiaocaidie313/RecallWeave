package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSubagentSessionJSONKeepsStoredContextPrivate(t *testing.T) {
	encoded, err := json.Marshal(SubagentSession{
		ID: "child", ConversationID: 7, Revision: 1,
		Snapshot: json.RawMessage(`{"secret":"SNAPSHOT_MARKER"}`),
		Preset:   json.RawMessage(`{"private":"PRESET_MARKER"}`),
		Sources:  json.RawMessage(`[{"private":"SOURCES_MARKER"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"SNAPSHOT_MARKER", "PRESET_MARKER", "SOURCES_MARKER", `"snapshot"`, `"preset"`, `"sources"`} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private stored context leaked through session metadata: %s", encoded)
		}
	}
	if !strings.Contains(string(encoded), `"id":"child"`) || !strings.Contains(string(encoded), `"conversation_id":7`) {
		t.Fatalf("public session identity missing: %s", encoded)
	}
}
