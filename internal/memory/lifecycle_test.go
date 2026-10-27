package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func lifecycleTestManager(t *testing.T) (*MemoryManger, sqlmock.Sqlmock) {
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
	return NewMemoryManger(db, nil), mock
}

func TestDeactivateMemoryPreservesOriginal(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "memories".*FOR UPDATE`).WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "session_id", "seq_start", "seq_end", "status", "content"}).AddRow(1, 10, 3, 4, "active", "旧方案"))
	mock.ExpectExec(`UPDATE "memories" SET `).
		WithArgs(sqlmock.AnyArg(), "用户要求停用", store.MemoryInactive, sqlmock.AnyArg(), 1, store.MemoryActive).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	item, err := manager.DeactivateMemory(context.Background(), 1, " 用户要求停用 ")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != store.MemoryInactive || item.DeactivatedAt == nil || item.DeactivationReason != "用户要求停用" || item.Content != "旧方案" || item.SessionID != 10 || item.SeqStart != 3 || item.SeqEnd != 4 {
		t.Fatalf("unexpected deactivation: %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeactivateMemoryRetryPreservesFirstReason(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	when := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "memories".*FOR UPDATE`).WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "deactivated_at", "deactivation_reason"}).AddRow(1, "inactive", when, "首次原因"))
	mock.ExpectCommit()
	item, err := manager.DeactivateMemory(context.Background(), 1, "重试原因")
	if err != nil || item.DeactivationReason != "首次原因" || item.DeactivatedAt == nil || !item.DeactivatedAt.Equal(when) {
		t.Fatalf("retry changed metadata: %+v %v", item, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeactivateMemoryFailuresDoNotReturnSuccess(t *testing.T) {
	for _, missing := range []bool{true, false} {
		manager, mock := lifecycleTestManager(t)
		mock.ExpectBegin()
		rows := sqlmock.NewRows([]string{"id", "status"})
		if !missing {
			rows.AddRow(1, "active")
		}
		mock.ExpectQuery(`SELECT .* FROM "memories".*FOR UPDATE`).WithArgs(1, 1).WillReturnRows(rows)
		if !missing {
			mock.ExpectExec(`UPDATE "memories" SET `).WillReturnError(errors.New("write failed"))
		}
		mock.ExpectRollback()
		item, err := manager.DeactivateMemory(context.Background(), 1, "原因")
		if err == nil || item.ID != 0 {
			t.Fatalf("false success: %+v %v", item, err)
		}
		if missing && !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("missing record: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeactivateMemoryRejectsInvalidArguments(t *testing.T) {
	manager := NewMemoryManger(nil, nil)
	for _, args := range []struct {
		id     uint
		reason string
	}{{0, "原因"}, {1, " "}} {
		if _, err := manager.DeactivateMemory(context.Background(), args.id, args.reason); !errors.Is(err, ErrInvalidDeactivation) {
			t.Fatalf("invalid request reached storage: %v", err)
		}
	}
}

func TestKeywordSearchVisibility(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE status = \$1.*LIMIT \$4`).
		WithArgs(store.MemoryActive, "%方案%", "%方案%", 25).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(2, "active"))
	items, err := manager.FindMemories(context.Background(), "方案", "", 5)
	if err != nil || len(items) != 1 || items[0].ID != 2 {
		t.Fatalf("default search: %+v %v", items, err)
	}
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE .*LIMIT \$3`).
		WithArgs("%方案%", "%方案%", 25).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(1, "inactive").AddRow(2, "active"))
	items, err = manager.FindMemories(context.Background(), "方案", "", 5, true)
	if err != nil || len(items) != 2 {
		t.Fatalf("history search: %+v %v", items, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCatalogVisibility(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE session_id = \$1 AND status = \$2`).
		WithArgs(10, store.MemoryActive).WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(2, "active"))
	if _, err := manager.ListBySession(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE session_id = \$1 ORDER BY`).
		WithArgs(10).WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(1, "inactive").AddRow(2, "active"))
	items, err := manager.ListBySession(context.Background(), 10, true)
	if err != nil || len(items) != 2 {
		t.Fatalf("management listing should preserve history: %+v %v", items, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
