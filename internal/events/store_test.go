package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func eventDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
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
	return db, mock
}

func TestAppendUsesCallerTransactionAndConversationLockBeforeIDAllocation(t *testing.T) {
	db, mock := eventDB(t)
	event := store.ConversationEvent{ConversationID: 17, Kind: "message.created", Key: "report:task-1", Data: json.RawMessage(`{"message_id":4}`)}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT "id" FROM "conversations" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).WithArgs(uint(17), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(17))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .*RETURNING "id"`).WithArgs(uint(17), "message.created", "report:task-1", `{"message_id":4}`, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(8))
	mock.ExpectCommit()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := LockConversation(tx, 17); err != nil {
			return err
		}
		return Append(tx, &event)
	})
	if err != nil || event.ID != 8 {
		t.Fatalf("append=%+v %v", event, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorMustBelongToRequestedConversation(t *testing.T) {
	db, mock := eventDB(t)
	reader := NewStore(db)
	if err := reader.ValidateCursor(context.Background(), 17, 0); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT count\(\*\) FROM "conversation_events" WHERE id = \$1 AND conversation_id = \$2`).WithArgs(uint64(22), uint(17)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	if err := reader.ValidateCursor(context.Background(), 17, 22); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("foreign cursor accepted: %v", err)
	}
	mock.ExpectQuery(`SELECT count\(\*\) FROM "conversation_events" WHERE id = \$1 AND conversation_id = \$2`).WithArgs(uint64(21), uint(17)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	if err := reader.ValidateCursor(context.Background(), 17, 21); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEventListScopesOwnerOrdersReplayAndBoundsBatch(t *testing.T) {
	db, mock := eventDB(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT \* FROM "conversation_events" WHERE conversation_id = \$1 AND id > \$2 ORDER BY id ASC LIMIT \$3`).WithArgs(uint(17), uint64(21), 100).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "kind", "key", "data", "created_at"}).AddRow(24, 17, "task.finished", "task:t1", `{"task_id":"t1","status":"completed"}`, now))
	rows, err := NewStore(db).List(context.Background(), 17, 21, 1000)
	if err != nil || len(rows) != 1 || rows[0].ID != 24 || string(rows[0].Data) != `{"task_id":"t1","status":"completed"}` {
		t.Fatalf("replay=%+v %v", rows, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEventsRejectMalformedAndCancelledOperationsBeforeSQL(t *testing.T) {
	db, mock := eventDB(t)
	reader := NewStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.List(ctx, 17, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := reader.List(context.Background(), 0, 0, 10); err == nil {
		t.Fatal("unscoped read accepted")
	}
	for _, event := range []store.ConversationEvent{{ConversationID: 17, Kind: "message.created\ninjected", Key: "k", Data: json.RawMessage(`{}`)}, {ConversationID: 17, Kind: "message.created", Key: "k", Data: json.RawMessage(`broken`)}, {ConversationID: 0, Kind: "message.created", Key: "k", Data: json.RawMessage(`{}`)}} {
		if err := Append(db, &event); err == nil {
			t.Fatal("malformed event accepted")
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
