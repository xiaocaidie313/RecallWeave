package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/run"
	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func runHTTPHandler(t *testing.T) (*RunHandler, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return NewRunHandler(run.NewRunManager(db)), mock
}

func TestRunDetailWorksWithoutConversationHandler(t *testing.T) {
	handler, mock := runHTTPHandler(t)
	mock.ExpectQuery(`SELECT .* FROM "agent_runs" WHERE run_id = .*ORDER BY .*LIMIT`).WithArgs("run-1", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "run_id", "conversation_id", "status", "content"}).
			AddRow(1, "run-1", 7, "completed", "saved answer"))
	router := NewRouter(nil, nil, nil, nil, nil, handler)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runs/run-1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("run detail route not connected: %d %s", recorder.Code, recorder.Body.String())
	}
	var result types.AgentRun
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.RunID != "run-1" || result.ConversationID != 7 || result.Content != "saved answer" {
		t.Fatalf("run detail contract changed: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunDetailMapsStorageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"missing run", nil, http.StatusNotFound},
		{"storage unavailable", errors.New("storage unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, mock := runHTTPHandler(t)
			query := mock.ExpectQuery(`SELECT .* FROM "agent_runs" WHERE run_id = .*ORDER BY .*LIMIT`).WithArgs("run-1", 1)
			if tc.err != nil {
				query.WillReturnError(tc.err)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"id"}))
			}
			recorder := httptest.NewRecorder()
			NewRouter(nil, nil, nil, nil, nil, handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runs/run-1", nil))
			if recorder.Code != tc.want {
				t.Fatalf("got HTTP %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunListWorksWithoutConversationHandler(t *testing.T) {
	handler, mock := runHTTPHandler(t)
	mock.ExpectQuery(`SELECT count\(\*\) FROM "conversations" WHERE id =`).WithArgs(7).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT .* FROM "agent_runs" WHERE conversation_id = .*ORDER BY id DESC LIMIT`).WithArgs(7, 20).
		WillReturnRows(sqlmock.NewRows([]string{"id", "run_id", "conversation_id", "status"}).AddRow(1, "run-1", 7, "completed"))
	recorder := httptest.NewRecorder()
	NewRouter(nil, nil, nil, nil, nil, handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/conversations/7/runs", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("run list route not connected: %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Runs []types.AgentRun `json:"runs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].RunID != "run-1" {
		t.Fatalf("run list contract changed: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunListRejectsInvalidConversationIDBeforeStorage(t *testing.T) {
	for _, id := range []string{"abc", "0"} {
		recorder := httptest.NewRecorder()
		NewRouter(nil, nil, nil, nil, nil, NewRunHandler(nil)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/conversations/"+id+"/runs", nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid conversation %q reached storage: %d %s", id, recorder.Code, recorder.Body.String())
		}
	}
}

func TestRunListDistinguishesEmptyAndMissingConversations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exists   int
		wantCode int
		wantBody string
	}{
		{"existing new conversation", 1, http.StatusOK, `{"runs":[]}`},
		{"missing conversation", 0, http.StatusNotFound, `{"error":"conversation not found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, mock := runHTTPHandler(t)
			mock.ExpectQuery(`SELECT count\(\*\) FROM "conversations" WHERE id =`).WithArgs(7).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.exists))
			if tc.exists == 1 {
				mock.ExpectQuery(`SELECT .* FROM "agent_runs" WHERE conversation_id = .*ORDER BY id DESC LIMIT`).WithArgs(7, 20).
					WillReturnRows(sqlmock.NewRows([]string{"id", "run_id", "conversation_id"}))
			}
			recorder := httptest.NewRecorder()
			NewRouter(nil, nil, nil, nil, nil, handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/conversations/7/runs", nil))
			if recorder.Code != tc.wantCode || recorder.Body.String() != tc.wantBody {
				t.Fatalf("got HTTP %d %s; want HTTP %d %s", recorder.Code, recorder.Body.String(), tc.wantCode, tc.wantBody)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunListMapsLookupFailuresToServerError(t *testing.T) {
	for _, failedLookup := range []string{"conversation", "runs"} {
		t.Run(failedLookup, func(t *testing.T) {
			handler, mock := runHTTPHandler(t)
			query := mock.ExpectQuery(`SELECT count\(\*\) FROM "conversations" WHERE id =`).WithArgs(7)
			if failedLookup == "conversation" {
				query.WillReturnError(errors.New("conversation storage unavailable"))
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
				mock.ExpectQuery(`SELECT .* FROM "agent_runs" WHERE conversation_id = .*ORDER BY id DESC LIMIT`).WithArgs(7, 20).
					WillReturnError(errors.New("run storage unavailable"))
			}
			recorder := httptest.NewRecorder()
			NewRouter(nil, nil, nil, nil, nil, handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/conversations/7/runs", nil))
			if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != `{"error":"failed to list runs"}` {
				t.Fatalf("storage error misreported: HTTP %d %s", recorder.Code, recorder.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
