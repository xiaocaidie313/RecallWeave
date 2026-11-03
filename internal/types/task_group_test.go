package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskGroupHidesInternalReportClaimAndRetainsMemberIDs(t *testing.T) {
	raw, err := json.Marshal(TaskGroup{ID: "group", ConversationID: 9, Goal: "Compare evidence", TaskIDs: json.RawMessage(`["task-a","task-b"]`), ReportClaim: "PRIVATE_CLAIM", ReportStatus: "processing"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_CLAIM") || strings.Contains(string(raw), "report_claim") || !strings.Contains(string(raw), `"task_ids":["task-a","task-b"]`) {
		t.Fatalf("group projection incorrect: %s", raw)
	}
}
