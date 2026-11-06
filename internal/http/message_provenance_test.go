package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
)

func provenanceMessage(id uint, seq int, role, text string) types.Message {
	return types.Message{ID: id, SessionID: 70, SourceTag: types.SourceChat, Seq: seq, Role: role, Content: text}
}

func provenancePayload(t *testing.T, message types.Message, metadata map[string]any) []byte {
	t.Helper()
	payload := map[string]any{"conversation_id": 7, "message": message}
	for key, value := range metadata {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func expectProvenanceHistory(mock sqlmock.Sqlmock, conversation bool, messages ...types.Message) {
	if conversation {
		mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE .*id.*ORDER BY .*LIMIT`).WithArgs(7, 1).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
		mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE conversation_id = .*source_tag = .*ORDER BY .*LIMIT`).WithArgs(7, types.SourceChat, 1).
			WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(70, 7, "chat"))
	} else {
		mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE .*id.*ORDER BY .*LIMIT`).WithArgs(70, 1).
			WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(70, 7, "chat"))
	}
	rows := sqlmock.NewRows([]string{"id", "session_id", "seq", "role", "content"})
	for _, message := range messages {
		rows.AddRow(message.ID, message.SessionID, message.Seq, message.Role, message.Content)
	}
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*ORDER BY seq`).WithArgs(70).WillReturnRows(rows)
}

func readProvenanceResponse(t *testing.T, path string, managerRouter http.Handler) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	managerRouter.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("history load failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Messages
}

func TestMessageHistoryRetypessSavedSSEProvenance(t *testing.T) {
	for _, conversation := range []bool{true, false} {
		path := "/api/sessions/70/messages"
		if conversation {
			path = "/api/conversations/7/messages"
		}
		t.Run(path, func(t *testing.T) {
			manager, messageStore, mock := forkHTTPManagers(t)
			question := provenanceMessage(700, 1, "user", "question")
			direct := provenanceMessage(701, 2, "assistant", "original result without inline URL")
			group := provenanceMessage(702, 3, "assistant", "combined saved result")
			expectProvenanceHistory(mock, conversation, question, direct, group)
			sources := []map[string]any{{"kind": "web", "tool": "mcp_tavily_search", "url": "https://example.test/original", "source_run_id": "child-run"}, {"kind": "memory", "tool": "get_messages", "session_id": 42, "seq_start": 4, "seq_end": 6, "source_run_id": "other-child"}}
			directMeta := map[string]any{"task_id": "task-1", "group_id": "group-1", "task_run_id": "child-run", "sources": sources}
			groupMeta := map[string]any{"group_id": "group-2", "run_id": "synthesis-run", "sources": sources}
			mock.ExpectQuery(`SELECT .* FROM "conversation_events" WHERE conversation_id = .*AND kind = .*AND key IN`).
				WithArgs(7, "message.created", "message:700", "message:701", "message:702").
				WillReturnRows(sqlmock.NewRows([]string{"conversation_id", "kind", "key", "data"}).
					AddRow(7, "message.created", "message:701", provenancePayload(t, direct, directMeta)).
					AddRow(7, "message.created", "message:702", provenancePayload(t, group, groupMeta)))
			var conversationHandler *ConversationHandler
			var sessionHandler *SessionHandler
			if conversation {
				conversationHandler = NewConversationHandler(manager, messageStore)
			} else {
				sessionHandler = NewSessionHandler(messageStore)
			}
			messages := readProvenanceResponse(t, path, NewRouter(nil, nil, nil, conversationHandler, sessionHandler, nil))
			if len(messages) != 3 {
				t.Fatalf("messages missing: %+v", messages)
			}
			for index, want := range []types.Message{question, direct, group} {
				if messages[index]["id"] != float64(want.ID) || messages[index]["session_id"] != float64(70) || messages[index]["content"] != want.Content {
					t.Fatalf("unstable identity/content: %+v", messages[index])
				}
			}
			if _, exists := messages[0]["sources"]; exists {
				t.Fatal("ordinary message invented sources")
			}
			for index, metadata := range []map[string]any{directMeta, groupMeta} {
				encoded, _ := json.Marshal(metadata)
				var expected map[string]any
				_ = json.Unmarshal(encoded, &expected)
				for key, want := range expected {
					if !reflect.DeepEqual(messages[index+1][key], want) {
						t.Fatalf("saved SSE field %s changed: got %v want %v", key, messages[index+1][key], want)
					}
				}
			}
			if _, exists := messages[1]["run_id"]; exists {
				t.Fatal("direct message invented a report run")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMessageHistoryRejectsMalformedAndForeignProvenance(t *testing.T) {
	message := provenanceMessage(701, 2, "assistant", "saved result")
	for _, scenario := range []string{"event owner", "payload owner", "message id", "session", "seq", "role", "content", "key", "kind", "invalid payload", "invalid sources"} {
		t.Run(scenario, func(t *testing.T) {
			manager, messageStore, mock := forkHTTPManagers(t)
			expectProvenanceHistory(mock, true, message)
			eventOwner, payloadOwner, key, kind := 7, 7, "message:701", "message.created"
			typesd := message
			metadata := map[string]any{"conversation_id": payloadOwner, "task_id": "must-not-leak", "sources": []map[string]any{{"kind": "web", "url": "https://private.test"}}}
			switch scenario {
			case "event owner":
				eventOwner = 8
			case "payload owner":
				metadata["conversation_id"] = 8
			case "message id":
				typesd.ID = 999
			case "session":
				typesd.SessionID = 80
			case "seq":
				typesd.Seq = 9
			case "role":
				typesd.Role = "user"
			case "content":
				typesd.Content = "different text"
			case "key":
				key = "message:999"
			case "kind":
				kind = "task.finished"
			case "invalid sources":
				metadata["sources"] = map[string]any{"url": "https://private.test"}
			}
			raw := provenancePayload(t, typesd, metadata)
			if scenario == "invalid payload" {
				raw = []byte(`["unexpected event payload"]`)
			}
			mock.ExpectQuery(`SELECT .* FROM "conversation_events" WHERE conversation_id = .*AND kind = .*AND key IN`).WithArgs(7, "message.created", "message:701").
				WillReturnRows(sqlmock.NewRows([]string{"conversation_id", "kind", "key", "data"}).AddRow(eventOwner, kind, key, raw))
			messages := readProvenanceResponse(t, "/api/conversations/7/messages", NewRouter(nil, nil, nil, NewConversationHandler(manager, messageStore), NewSessionHandler(messageStore), nil))
			if len(messages) != 1 || messages[0]["content"] != message.Content {
				t.Fatalf("ordinary history damaged: %+v", messages)
			}
			if _, exists := messages[0]["task_id"]; exists {
				t.Fatalf("invalid event leaked metadata: %+v", messages[0])
			}
			if _, exists := messages[0]["sources"]; exists {
				t.Fatalf("invalid event leaked sources: %+v", messages[0])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMessageHistoryReportsProvenanceStorageFailure(t *testing.T) {
	_, messageStore, mock := forkHTTPManagers(t)
	message := provenanceMessage(701, 2, "assistant", "saved result")
	expectProvenanceHistory(mock, false, message)
	mock.ExpectQuery(`SELECT .* FROM "conversation_events" WHERE conversation_id = .*AND kind = .*AND key IN`).WithArgs(7, "message.created", "message:701").WillReturnError(errors.New("storage unavailable"))
	recorder := httptest.NewRecorder()
	NewRouter(nil, nil, nil, nil, NewSessionHandler(messageStore), nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/sessions/70/messages", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("silently lost provenance: %d %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
