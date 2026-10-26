package extract

import (
	"context"
	"errors"
	"testing"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestFullReextractionPreservesReplacementHistory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    store.MemoryStatus
		protected bool
	}{
		{"inactive memory", store.MemoryInactive, true},
		{"active memory", store.MemoryActive, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT .* FROM "memories".*FOR UPDATE`).WithArgs(10).
				WillReturnRows(sqlmock.NewRows([]string{"id", "session_id", "status"}).AddRow(1, 10, tc.status))
			if tc.protected {
				mock.ExpectRollback()
			} else {
				mock.ExpectExec(`DELETE FROM "memories" WHERE session_id =`).WithArgs(10).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			err = NewExtractExcuter(db, nil, nil).replaceMemories(context.Background(), 10, nil)
			if tc.protected && !errors.Is(err, store.ErrMemoryHistoryProtected) {
				t.Fatalf("history was not protected: %v", err)
			}
			if !tc.protected && err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
