package chat_test

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/chat"
	apphttp "recallweave/internal/http"
	"recallweave/internal/llm"
	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
)

func noDeliveryModel(t *testing.T) (*chat.Agent, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	model := &dispatchModel{next: func(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
		calls.Add(1)
		return llm.Turn{}, errors.New("direct delivery must not call the model")
	}}
	return chat.NewAgent(agent.NewAgent(1, "recall", model, "", 2), nil, nil), calls
}

func TestDefaultDirectDeliveryPublishesOriginalResultAndSourcesOverSSE(t *testing.T) {
	service, calls := noDeliveryModel(t)
	id := strings.Repeat("a", 32)
	task := dispatchTask(1, id)
	task.DeliveryMode = ""
	ledger := &dispatchLedger{rows: map[string]types.SubagentTask{id: task}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	apphttp.MountConversationEvents(router, apphttp.NewEventsHandler(ledger))
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/conversations/1/events", nil)
	stream, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	startDispatcher(t, service, ledger)
	row := waitReport(t, ledger, id, "delivered")
	if row.ReportRunID != "" {
		t.Fatalf("direct delivery created a report run: %+v", row)
	}
	scanner := bufio.NewScanner(stream.Body)
	var data string
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") && strings.Contains(scanner.Text(), "VERIFIED_FINDINGS") {
			data = scanner.Text()
			break
		}
	}
	if !strings.Contains(data, "sources") || !strings.Contains(data, "https://example.test/source") || !strings.Contains(data, "task_run_id") || !strings.Contains(data, task.RunID) {
		t.Fatalf("direct SSE lost result provenance: %s (stream error: %v)", data, scanner.Err())
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if calls.Load() != 0 || len(ledger.runs) != 0 || len(ledger.messages[1]) != 1 || ledger.messages[1][0].Content != "VERIFIED_FINDINGS" {
		t.Fatalf("direct delivery was regenerated: calls=%d runs=%d messages=%+v", calls.Load(), len(ledger.runs), ledger.messages[1])
	}
}

func TestDirectGroupDeliversMembersIndependentlyWithoutFinalReport(t *testing.T) {
	service, calls := noDeliveryModel(t)
	ledger, id, ids := newGroupLedger()
	group := ledger.groups[id]
	group.DeliveryMode, group.ReportStatus = "direct", "not_required"
	ledger.groups[id] = group
	first, second := ledger.rows[ids[0]], ledger.rows[ids[1]]
	first.ReportStatus, second.ReportStatus = "pending", ""
	ledger.rows[ids[0]], ledger.rows[ids[1]] = first, second
	startGroupDispatcher(t, service, ledger)
	waitReport(t, &ledger.dispatchLedger, ids[0], "delivered")
	ledger.mu.Lock()
	if ledger.groups[id].Status != "running" || len(ledger.messages[1]) != 1 || calls.Load() != 0 {
		ledger.mu.Unlock()
		t.Fatal("first direct result waited for its sibling")
	}
	second.Status, second.Error, second.ReportStatus = "failed", "LOOKUP_FAILED", "pending"
	ledger.rows[ids[1]] = second
	group.Status = "partial"
	ledger.groups[id] = group
	ledger.mu.Unlock()
	waitReport(t, &ledger.dispatchLedger, ids[1], "delivered")
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if calls.Load() != 0 || len(ledger.runs) != 0 || len(ledger.messages[1]) != 2 || ledger.groups[id].ReportStatus != "not_required" {
		t.Fatalf("direct members also scheduled synthesis: calls=%d messages=%d group=%+v", calls.Load(), len(ledger.messages[1]), ledger.groups[id])
	}
	if !strings.Contains(ledger.messages[1][1].Content, "LOOKUP_FAILED") {
		t.Fatal("failed sibling was hidden")
	}
	for _, event := range ledger.events {
		if event.Kind == "message.created" && !strings.Contains(string(event.Data), id) {
			t.Fatal("direct member SSE lost group identity")
		}
	}
}

func TestCombinedAndLegacyGroupDeliverSavedResultsWithoutModel(t *testing.T) {
	for _, mode := range []string{"combined", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			service, calls := noDeliveryModel(t)
			ledger, id, ids := newGroupLedger()
			group := ledger.groups[id]
			group.DeliveryMode = mode
			ledger.groups[id] = group
			startGroupDispatcher(t, service, ledger)
			// The first result stays grouped while its sibling has not terminated.
			ledger.mu.Lock()
			if ledger.rows[ids[0]].ReportStatus != "grouped" || len(ledger.messages[1]) != 0 {
				ledger.mu.Unlock()
				t.Fatal("group delivered prematurely")
			}
			second := ledger.rows[ids[1]]
			second.Status, second.Error = "failed", "LOOKUP_FAILED"
			ledger.rows[ids[1]] = second
			group.Status, group.ReportStatus = "partial", "pending"
			ledger.groups[id] = group
			ledger.mu.Unlock()
			row := waitGroupReport(t, ledger, id, "delivered")
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if calls.Load() != 0 || len(ledger.runs) != 0 || row.ReportRunID != "" || len(ledger.messages[1]) != 1 {
				t.Fatalf("combined delivery created a new model execution: calls=%d group=%+v", calls.Load(), row)
			}
			content := ledger.messages[1][0].Content
			if !strings.Contains(content, "VERIFIED_FINDINGS") || !strings.Contains(content, "LOOKUP_FAILED") {
				t.Fatalf("combined output lost a member: %s", content)
			}
		})
	}
}
