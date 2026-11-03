package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"recallweave/internal/events"
	"recallweave/internal/types"

	"github.com/gin-gonic/gin"
)

type fakeEventReader struct {
	rows   []types.ConversationEvent
	exists bool
	err    error
	owner  uint
	cursor uint64
	lists  int
	list   func(context.Context, uint, uint64, int) ([]types.ConversationEvent, error)
}

func (f *fakeEventReader) ConversationExists(_ context.Context, owner uint) (bool, error) {
	f.owner = owner
	return f.exists, f.err
}
func (f *fakeEventReader) ValidateCursor(_ context.Context, owner uint, after uint64) error {
	f.owner, f.cursor = owner, after
	if after == 0 {
		return nil
	}
	for _, row := range f.rows {
		if row.ID == after && row.ConversationID == owner {
			return nil
		}
	}
	return events.ErrInvalidCursor
}
func (f *fakeEventReader) List(ctx context.Context, owner uint, after uint64, limit int) ([]types.ConversationEvent, error) {
	f.lists++
	if f.list != nil {
		return f.list(ctx, owner, after, limit)
	}
	var rows []types.ConversationEvent
	for _, row := range f.rows {
		if row.ConversationID == owner && row.ID > after {
			rows = append(rows, row)
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}

type cancellingEventWriter struct {
	*httptest.ResponseRecorder
	when   string
	cancel context.CancelFunc
}

type deadlineEventWriter struct {
	*cancellingEventWriter
	deadlines []time.Time
	current   time.Time
}

func (w *deadlineEventWriter) SetWriteDeadline(deadline time.Time) error {
	w.current = deadline
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *cancellingEventWriter) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), w.when) {
		w.cancel()
	}
}

func eventRouter(reader ConversationEventReader) (*gin.Engine, *EventsHandler) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewEventsHandler(reader)
	MountConversationEvents(router, handler)
	return router, handler
}

func TestConversationEventsReplayUsesLatestHeaderCursorAndKeepsOwnerScope(t *testing.T) {
	reader := &fakeEventReader{exists: true, rows: []types.ConversationEvent{
		{ID: 1, ConversationID: 17, Kind: "task.finished", Data: json.RawMessage(`{"status":"completed"}`)},
		{ID: 2, ConversationID: 18, Kind: "message.created", Data: json.RawMessage(`{"content":"OTHER_PRIVATE_CONVERSATION"}`)},
		{ID: 3, ConversationID: 17, Kind: "message.created", Data: json.RawMessage("{\n  \"content\": \"saved reply\"\n}")},
	}}
	router, _ := eventRouter(reader)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/conversations/17/events?after=0", nil).WithContext(ctx)
	request.Header.Set("Last-Event-ID", "1")
	writer := &cancellingEventWriter{ResponseRecorder: httptest.NewRecorder(), when: "id: 3\n", cancel: cancel}
	router.ServeHTTP(writer, request)
	body := writer.Body.String()
	if writer.Code != http.StatusOK || !writer.Flushed || reader.owner != 17 || reader.cursor != 1 {
		t.Fatalf("stream not scoped/flushed: status=%d owner=%d cursor=%d", writer.Code, reader.owner, reader.cursor)
	}
	if !strings.Contains(writer.Header().Get("Content-Type"), "text/event-stream") || writer.Header().Get("Cache-Control") != "no-cache" || writer.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("missing SSE headers")
	}
	if !strings.Contains(body, "retry: 3000\n\n") || !strings.Contains(body, "id: 3\nevent: message.created\ndata: {\"content\":\"saved reply\"}\n\n") || strings.Contains(body, "id: 1\n") || strings.Contains(body, "OTHER_PRIVATE") {
		t.Fatalf("invalid replay frame: %s", body)
	}
}

func TestConversationEventsRejectInvalidScopeCursorAndStorageErrorsBeforeStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, path, header string
		exists             bool
		err                error
		status             int
	}{
		{"invalid owner", "/api/conversations/0/events", "", true, nil, 400},
		{"invalid query cursor", "/api/conversations/17/events?after=-1", "", true, nil, 400},
		{"duplicate query cursor", "/api/conversations/17/events?after=0&after=1", "", true, nil, 400},
		{"invalid header cursor", "/api/conversations/17/events", "not-a-number", true, nil, 400},
		{"unknown conversation", "/api/conversations/17/events", "", false, nil, 404},
		{"foreign cursor", "/api/conversations/17/events", "9", true, nil, 400},
		{"storage failure", "/api/conversations/17/events", "", true, errors.New("PRIVATE_SQL_ERROR"), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeEventReader{exists: tc.exists, err: tc.err, rows: []types.ConversationEvent{{ID: 9, ConversationID: 18}}}
			router, _ := eventRouter(reader)
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Header.Set("Last-Event-ID", tc.header)
			writer := httptest.NewRecorder()
			router.ServeHTTP(writer, request)
			if writer.Code != tc.status || strings.Contains(writer.Header().Get("Content-Type"), "text/event-stream") || strings.Contains(writer.Body.String(), "PRIVATE_SQL_ERROR") {
				t.Fatalf("bad preflight: %d %s", writer.Code, writer.Body)
			}
		})
	}
}

func TestConversationEventsHeartbeatAndDisconnectDoNotWaitForNextPoll(t *testing.T) {
	reader := &fakeEventReader{exists: true}
	router, handler := eventRouter(reader)
	handler.pollInterval = time.Hour
	handler.heartbeatInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancellingEventWriter{ResponseRecorder: httptest.NewRecorder(), when: ": keep-alive\n\n", cancel: cancel}
	started := time.Now()
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/conversations/17/events", nil).WithContext(ctx))
	if !strings.Contains(writer.Body.String(), ": keep-alive\n\n") || reader.lists != 1 || time.Since(started) > time.Second {
		t.Fatalf("heartbeat/disconnect failed: lists=%d body=%s", reader.lists, writer.Body)
	}
}

func TestConversationEventsPollsNewDurableMessages(t *testing.T) {
	reader := &fakeEventReader{exists: true}
	reader.list = func(ctx context.Context, owner uint, after uint64, limit int) ([]types.ConversationEvent, error) {
		if owner != 17 || after != 0 || limit != 100 {
			return nil, fmt.Errorf("wrong poll scope")
		}
		if reader.lists == 1 {
			return nil, nil
		}
		return []types.ConversationEvent{{ID: 5, ConversationID: 17, Kind: "message.created", Data: json.RawMessage(`{"content":"new durable report"}`)}}, nil
	}
	router, handler := eventRouter(reader)
	handler.pollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancellingEventWriter{ResponseRecorder: httptest.NewRecorder(), when: "id: 5\n", cancel: cancel}
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/conversations/17/events", nil).WithContext(ctx))
	if reader.lists != 2 || !strings.Contains(writer.Body.String(), "new durable report") {
		t.Fatalf("new event not delivered: lists=%d %s", reader.lists, writer.Body)
	}
}

func TestConversationEventsRejectsMalformedOrForeignReaderResults(t *testing.T) {
	for _, row := range []types.ConversationEvent{{ID: 1, ConversationID: 18, Kind: "message.created", Data: json.RawMessage(`{}`)}, {ID: 1, ConversationID: 17, Kind: "message.created\ninjected", Data: json.RawMessage(`{}`)}, {ID: 1, ConversationID: 17, Kind: "message.created", Data: json.RawMessage(`broken`)}} {
		reader := &fakeEventReader{exists: true, list: func(context.Context, uint, uint64, int) ([]types.ConversationEvent, error) {
			return []types.ConversationEvent{row}, nil
		}}
		router, _ := eventRouter(reader)
		writer := httptest.NewRecorder()
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/conversations/17/events", nil))
		if writer.Code != 500 || strings.Contains(writer.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("invalid event streamed: %d %s", writer.Code, writer.Body)
		}
	}
}

func TestConversationEventsCatchUpAcrossBatchesWithoutWaitingForPoll(t *testing.T) {
	reader := &fakeEventReader{exists: true}
	for i := 1; i <= 101; i++ {
		reader.rows = append(reader.rows, types.ConversationEvent{ID: uint64(i), ConversationID: 17, Kind: "task.finished", Data: json.RawMessage(`{}`)})
	}
	router, handler := eventRouter(reader)
	handler.pollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancellingEventWriter{ResponseRecorder: httptest.NewRecorder(), when: "id: 101\n", cancel: cancel}
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/conversations/17/events", nil).WithContext(ctx))
	if reader.lists != 2 || strings.Count(writer.Body.String(), "event: task.finished") != 101 {
		t.Fatalf("catchup failed: lists=%d", reader.lists)
	}
}

func TestConversationEventsClearWriteDeadlineBeforeIdlePolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &deadlineEventWriter{cancellingEventWriter: &cancellingEventWriter{ResponseRecorder: httptest.NewRecorder(), when: "id: 5\n", cancel: cancel}}
	reader := &fakeEventReader{exists: true}
	reader.list = func(context.Context, uint, uint64, int) ([]types.ConversationEvent, error) {
		if !writer.current.IsZero() {
			return nil, fmt.Errorf("write deadline leaked into idle polling")
		}
		if reader.lists == 1 {
			return nil, nil
		}
		return []types.ConversationEvent{{ID: 5, ConversationID: 17, Kind: "message.created", Data: json.RawMessage(`{}`)}}, nil
	}
	router, handler := eventRouter(reader)
	handler.pollInterval = time.Millisecond
	handler.heartbeatInterval = time.Hour
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/conversations/17/events", nil).WithContext(ctx))
	if !strings.Contains(writer.Body.String(), "id: 5\n") || len(writer.deadlines) < 4 || writer.deadlines[0].IsZero() || !writer.deadlines[1].IsZero() || writer.deadlines[2].IsZero() || !writer.deadlines[3].IsZero() || !writer.current.IsZero() {
		t.Fatalf("write deadlines not scoped to frames: %v body=%s", writer.deadlines, writer.Body)
	}
}
