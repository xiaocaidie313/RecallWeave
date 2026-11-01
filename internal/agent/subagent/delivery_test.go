package subagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/delivery"
	"recallweave/internal/llm"
)

func TestBackgroundDeliveryDefaultsDirectAndFollowupCanChooseSynthesis(t *testing.T) {
	m := backgroundManager(t, &memoryTaskStore{}, &continuationMemoryStore{})
	parent := managerParent(&transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "ORIGINAL_RESULT"}, nil
	}})
	first, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "a task"})
	if err != nil {
		t.Fatal(err)
	}
	if first.DeliveryMode != delivery.Direct {
		t.Fatalf("default mode=%q", first.DeliveryMode)
	}
	view := waitBackground(t, m, parent, first.TaskID, "completed")
	deadline := time.Now().Add(time.Second)
	for !view.Result.CanFollowUp && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		view, err = m.GetTask(context.Background(), parent, first.TaskID)
		if err != nil {
			t.Fatal(err)
		}
	}
	followup, err := m.SubmitFollowUp(context.Background(), parent, runtime.FollowupRequest{SubagentID: first.SubagentID, Message: "supplement", DeliveryMode: delivery.Synthesize})
	if err != nil {
		t.Fatal(err)
	}
	if followup.DeliveryMode != delivery.Synthesize {
		t.Fatalf("followup mode lost: %+v", followup)
	}
	waitBackground(t, m, parent, followup.TaskID, "completed")
	if _, err := m.Submit(context.Background(), parent, runtime.DelegationRequest{Task: "bad mode", DeliveryMode: "automatic"}); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestGroupDefaultDirectDeliversFirstMemberBeforeSibling(t *testing.T) {
	release := make(chan struct{})
	model := &transcriptModel{next: func(ctx context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
		snapshot, _ := conv.Snapshot()
		if strings.Contains(string(snapshot.Messages), "MEMBER_B") {
			select {
			case <-release:
			case <-ctx.Done():
				return llm.Turn{}, ctx.Err()
			}
		}
		return llm.Turn{Content: "original findings"}, nil
	}}
	m := groupManager(t, &memoryGroupStore{}, 2, 1)
	parent := managerParent(model)
	request := groupRequest()
	request.DeliveryMode = ""
	handle, err := m.SubmitGroup(context.Background(), parent, request)
	if err != nil {
		t.Fatal(err)
	}
	if handle.DeliveryMode != delivery.Direct || handle.ReportStatus != "not_required" {
		t.Fatalf("unexpected default group receipt: %+v", handle)
	}
	waitBackground(t, m, parent, handle.TaskIDs[0], "completed")
	view, err := m.GetGroup(context.Background(), parent, handle.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.Tasks[0].ReportStatus != "pending" || view.Tasks[1].ReportStatus == "pending" {
		t.Fatalf("first direct member waited for sibling: %+v", view)
	}
	close(release)
	view = waitGroup(t, m, parent, handle.GroupID, "completed")
	if view.ReportStatus != "not_required" {
		t.Fatalf("direct group scheduled another final report: %+v", view)
	}
}

func TestCombinedGroupStoresModeAndWaitsForBoth(t *testing.T) {
	m := groupManager(t, &memoryGroupStore{}, 2, 1)
	parent := managerParent(&transcriptModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		return llm.Turn{Content: "original"}, nil
	}})
	request := groupRequest()
	request.DeliveryMode = delivery.Combined
	handle, err := m.SubmitGroup(context.Background(), parent, request)
	if err != nil {
		t.Fatal(err)
	}
	view := waitGroup(t, m, parent, handle.GroupID, "completed")
	if view.DeliveryMode != delivery.Combined || view.ReportStatus != "pending" || view.Tasks[0].ReportStatus != "grouped" || view.Tasks[1].ReportStatus != "grouped" {
		t.Fatalf("combined mode was not preserved: %+v", view)
	}
}
