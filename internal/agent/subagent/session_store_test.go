package subagent

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var sessionStoreNow = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

func sessionStoreTestDB(t *testing.T) (*GormSubagentSessionStore, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	database, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Silent),
		NowFunc:              func() time.Time { return sessionStoreNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewSubagentSessionStore(database), mock
}

func sessionStoreRecord() store.SubagentSession {
	return store.SubagentSession{
		ID: "child-session", ConversationID: 9, AgentName: "researcher", Task: "Check original sources",
		LatestRunID: "run-1", Status: "completed",
		Snapshot: json.RawMessage(`{"version":1,"messages":[{"role":"user","content":"task"}]}`),
		Preset:   json.RawMessage(`{"Name":"researcher","AllowedTools":["get_messages"]}`),
		Sources:  json.RawMessage(`[{"kind":"message","session_id":8,"seq_start":2,"seq_end":2}]`),
	}
}

// GORM's JSON serializer must hand PostgreSQL JSON text, not raw []byte that
// could be bound as bytea or a quoted JSON string containing another object.
type sessionJSONArgument string

func (want sessionJSONArgument) Match(value driver.Value) bool {
	actual, ok := value.(string)
	if !ok {
		return false
	}
	var left, right any
	return json.Unmarshal([]byte(actual), &left) == nil && json.Unmarshal([]byte(want), &right) == nil && reflect.DeepEqual(left, right)
}

func TestSessionStoreCreatesNewIdentityWithJSONAndRevision(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	mock.ExpectBegin()
	mock.ExpectExec(`^INSERT INTO "subagent_sessions" .* VALUES .*`).WithArgs(
		record.ID, record.ConversationID, record.AgentName, record.Task, record.LatestRunID, record.Status,
		sessionJSONArgument(record.Snapshot), sessionJSONArgument(record.Preset), sessionJSONArgument(record.Sources),
		uint64(1), sessionStoreNow, sessionStoreNow,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repository.Create(context.Background(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Revision != 1 || !record.CreatedAt.Equal(sessionStoreNow) || !record.UpdatedAt.Equal(sessionStoreNow) {
		t.Fatalf("new context did not get its initial revision/timestamps: %+v", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreCreateDoesNotOverwriteExistingSession(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	duplicate := errors.New("duplicate key violates primary key constraint")
	mock.ExpectBegin()
	mock.ExpectExec(`^INSERT INTO "subagent_sessions" .* VALUES .*`).WillReturnError(duplicate)
	mock.ExpectRollback()
	if err := repository.Create(context.Background(), &record); !errors.Is(err, duplicate) {
		t.Fatalf("duplicate identity was not rejected: %v", err)
	}
	if record.Revision != 0 || !record.CreatedAt.IsZero() {
		t.Fatalf("failed creation mutated the caller: %+v", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func sessionStoreRows(records ...store.SubagentSession) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "conversation_id", "agent_name", "task", "latest_run_id", "status", "snapshot", "preset", "sources", "revision", "created_at", "updated_at"})
	for _, record := range records {
		rows.AddRow(record.ID, record.ConversationID, record.AgentName, record.Task, record.LatestRunID, record.Status,
			string(record.Snapshot), string(record.Preset), string(record.Sources), record.Revision, sessionStoreNow, sessionStoreNow)
	}
	return rows
}

func TestSessionStoreLoadRequiresOwnerAndReadsJSONStrings(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	want := sessionStoreRecord()
	want.Revision = 3
	mock.ExpectQuery(`SELECT .* FROM "subagent_sessions" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`).
		WithArgs(want.ID, want.ConversationID, 1).WillReturnRows(sessionStoreRows(want))
	got, err := repository.Load(context.Background(), want.ID, want.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.ConversationID != want.ConversationID || got.Revision != 3 ||
		!reflect.DeepEqual(got.Snapshot, want.Snapshot) || !reflect.DeepEqual(got.Preset, want.Preset) || !reflect.DeepEqual(got.Sources, want.Sources) {
		t.Fatalf("stored JSON was not restored: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreLoadCannotFallBackOutsideConversation(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	mock.ExpectQuery(`SELECT .* FROM "subagent_sessions" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`).
		WithArgs("child-session", uint(100), 1).WillReturnRows(sessionStoreRows())
	if _, err := repository.Load(context.Background(), "child-session", 100); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("foreign session lookup did not remain scoped: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectSessionUpdate(mock sqlmock.Sqlmock, record store.SubagentSession, expected uint64, affected int64) {
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "subagent_sessions" SET .* WHERE id = \$10 AND conversation_id = \$11 AND revision = \$12`).WithArgs(
		record.AgentName, record.Task, record.LatestRunID, record.Status,
		sessionJSONArgument(record.Snapshot), sessionJSONArgument(record.Preset), sessionJSONArgument(record.Sources),
		expected+1, sessionStoreNow, record.ID, record.ConversationID, expected,
	).WillReturnResult(sqlmock.NewResult(0, affected))
	mock.ExpectCommit()
}

func TestSessionStoreUpdateUsesRevisionCASAndJSONSerializer(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	record.Revision = 4
	record.LatestRunID = "run-2"
	record.Snapshot = json.RawMessage(`{"version":1,"messages":[{"role":"user","content":"continued"}]}`)
	expectSessionUpdate(mock, record, 4, 1)
	if err := repository.Update(context.Background(), &record, 4); err != nil {
		t.Fatal(err)
	}
	if record.Revision != 5 || !record.UpdatedAt.Equal(sessionStoreNow) {
		t.Fatalf("successful CAS did not update the caller's revision: %+v", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreUpdateConflictDoesNotAdvanceCallerRevision(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	record.Revision = 4
	expectSessionUpdate(mock, record, 4, 0)
	if err := repository.Update(context.Background(), &record, 4); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("stale session update was not rejected: %v", err)
	}
	if record.Revision != 4 || !record.UpdatedAt.IsZero() {
		t.Fatalf("conflict mutated the caller: %+v", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreUpdateCanClearUnusableCheckpoint(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	record.Revision = 1
	record.Snapshot = nil
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "subagent_sessions" SET .* WHERE id = \$10 AND conversation_id = \$11 AND revision = \$12`).WithArgs(
		record.AgentName, record.Task, record.LatestRunID, record.Status, nil,
		sessionJSONArgument(record.Preset), sessionJSONArgument(record.Sources), uint64(2), sessionStoreNow, record.ID, record.ConversationID, uint64(1),
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repository.Update(context.Background(), &record, 1); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreListIsScopedAndBounded(t *testing.T) {
	for _, test := range []struct {
		name               string
		requested, bounded int
	}{
		{"default", 0, 5}, {"explicit", 3, 3}, {"cap", 1000, 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, mock := sessionStoreTestDB(t)
			record := sessionStoreRecord()
			record.Revision = 1
			mock.ExpectQuery(`SELECT .* FROM "subagent_sessions" WHERE conversation_id = \$1 ORDER BY updated_at DESC,id DESC LIMIT \$2`).
				WithArgs(record.ConversationID, test.bounded).WillReturnRows(sessionStoreRows(record))
			rows, err := repository.List(context.Background(), record.ConversationID, test.requested)
			if err != nil || len(rows) != 1 || rows[0].ConversationID != record.ConversationID {
				t.Fatalf("unexpected scoped list: %+v %v", rows, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSessionStoreRejectsInvalidScopeAndJSONBeforeSQL(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	for _, test := range []struct {
		id    string
		owner uint
	}{{"", 9}, {"child", 0}, {" child ", 9}} {
		record := sessionStoreRecord()
		record.ID, record.ConversationID = test.id, test.owner
		if err := repository.Create(context.Background(), &record); err == nil {
			t.Fatal("invalid create accepted")
		}
		if _, err := repository.Load(context.Background(), test.id, test.owner); err == nil {
			t.Fatal("invalid load accepted")
		}
		if err := repository.Update(context.Background(), &record, 1); err == nil {
			t.Fatal("invalid update accepted")
		}
	}
	if _, err := repository.List(context.Background(), 0, 5); err == nil {
		t.Fatal("unscoped list accepted")
	}
	if err := repository.Create(context.Background(), nil); err == nil {
		t.Fatal("nil create accepted")
	}
	if err := repository.Update(context.Background(), nil, 1); err == nil {
		t.Fatal("nil update accepted")
	}
	record := sessionStoreRecord()
	for _, revision := range []uint64{0, math.MaxUint64} {
		if err := repository.Update(context.Background(), &record, revision); err == nil {
			t.Fatal("invalid revision accepted")
		}
	}
	record.Preset = json.RawMessage(`{`)
	if err := repository.Create(context.Background(), &record); err == nil {
		t.Fatal("invalid JSON create accepted")
	}
	if err := repository.Update(context.Background(), &record, 1); err == nil {
		t.Fatal("invalid JSON update accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreUnavailableAndCancellationFailClosed(t *testing.T) {
	for _, repository := range []*GormSubagentSessionStore{nil, NewSubagentSessionStore(nil)} {
		record := sessionStoreRecord()
		if err := repository.Create(context.Background(), &record); err == nil {
			t.Fatal("unavailable create accepted")
		}
		if _, err := repository.Load(context.Background(), record.ID, record.ConversationID); err == nil {
			t.Fatal("unavailable load accepted")
		}
		if err := repository.Update(context.Background(), &record, 1); err == nil {
			t.Fatal("unavailable update accepted")
		}
		if _, err := repository.List(context.Background(), record.ConversationID, 1); err == nil {
			t.Fatal("unavailable list accepted")
		}
	}
	repository, mock := sessionStoreTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record := sessionStoreRecord()
	if err := repository.Create(ctx, &record); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := repository.Load(ctx, record.ID, record.ConversationID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := repository.Update(ctx, &record, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := repository.List(ctx, record.ConversationID, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectCheckpointQuery(mock sqlmock.Sqlmock, record store.SubagentSession) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`SELECT count\(\*\) FROM "agent_runs" WHERE .*subagent_id = \$1 AND conversation_id = \$2 AND previous_run_id = \$3 AND run_id <> \$4.*COALESCE\(state->>'context_persist_failed', 'false'\) = 'true'`).
		WithArgs(record.ID, record.ConversationID, record.LatestRunID, record.LatestRunID)
}

func TestSessionStoreCheckpointVerifierRejectsPersistedFailure(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	expectCheckpointQuery(mock, record).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	if err := repository.CheckCheckpoint(context.Background(), record); !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("failed newer work did not invalidate the old checkpoint: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreCheckpointVerifierAcceptsCurrentCheckpoint(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	// The exact preceding checkpoint matters: a failed/CAS-conflicting run based
	// on an older version does not block a successfully advanced checkpoint.
	record.LatestRunID = "winning-new-run"
	expectCheckpointQuery(mock, record).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	if err := repository.CheckCheckpoint(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreCheckpointVerifierPropagatesDatabaseFailure(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	record := sessionStoreRecord()
	unavailable := errors.New("database unavailable")
	expectCheckpointQuery(mock, record).WillReturnError(unavailable)
	if err := repository.CheckCheckpoint(context.Background(), record); !errors.Is(err, unavailable) {
		t.Fatalf("verification failure was ignored: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreCheckpointVerifierRejectsInvalidOrUnavailableScope(t *testing.T) {
	repository, mock := sessionStoreTestDB(t)
	for _, change := range []func(*store.SubagentSession){
		func(record *store.SubagentSession) { record.ID = "" },
		func(record *store.SubagentSession) { record.ConversationID = 0 },
		func(record *store.SubagentSession) { record.LatestRunID = "" },
		func(record *store.SubagentSession) { record.LatestRunID = " run " },
	} {
		record := sessionStoreRecord()
		change(&record)
		if err := repository.CheckCheckpoint(context.Background(), record); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	}
	for _, unavailable := range []*GormSubagentSessionStore{nil, NewSubagentSessionStore(nil)} {
		if err := unavailable.CheckCheckpoint(context.Background(), sessionStoreRecord()); err == nil {
			t.Fatal("unavailable checkpoint verifier accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repository.CheckCheckpoint(ctx, sessionStoreRecord()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
