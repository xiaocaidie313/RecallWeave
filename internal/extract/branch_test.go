package extract

import (
	"context"
	"errors"
	"testing"

	"recallweave/internal/llm"
	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const scopeQuery = `SELECT .*conversations.parent_conversation_id FROM "sessions" LEFT JOIN conversations ON conversations.id = sessions.conversation_id WHERE sessions.id = \$1 LIMIT \$2`

func branchTestExtractor(t *testing.T) (*ExtractExcuter, sqlmock.Sqlmock, *branchModelProbe) {
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
	probe := &branchModelProbe{}
	return NewExtractExcuter(db, probe, probe), mock, probe
}

type branchModelProbe struct{ calls int }

func (p *branchModelProbe) Segment(context.Context, []llm.Message, []string, []string) ([]llm.Segment, error) {
	p.calls++
	return nil, errors.New("model must not be called")
}

func (p *branchModelProbe) Embed(context.Context, string) ([]float64, error) {
	p.calls++
	return nil, errors.New("model must not be called")
}

func scopeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"session_id", "conversation_id", "found_conversation_id", "parent_conversation_id"})
}

func TestBranchExtractionBlockedAtBothPublicEntrypoints(t *testing.T) {
	for _, mode := range []string{"full", "incremental"} {
		t.Run(mode, func(t *testing.T) {
			extractor, mock, probe := branchTestExtractor(t)
			mock.ExpectQuery(scopeQuery).WithArgs(10, 1).WillReturnRows(scopeRows().AddRow(10, 3, 3, 1))
			mock.ExpectBegin()
			mock.ExpectExec(`UPDATE "sessions" SET `).
				WithArgs("", store.ExtractSkipped, sqlmock.AnyArg(), 10).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			var count int
			var err error
			if mode == "full" {
				count, err = extractor.ExtractSessionToMemory(context.Background(), 10)
			} else {
				count, err = extractor.ExtractNewToMemory(context.Background(), 10)
			}
			if count != 0 || !errors.Is(err, store.ErrBranchMemoryPrivate) || probe.calls != 0 {
				t.Fatalf("branch extraction escaped guard: count=%d err=%v model_calls=%d", count, err, probe.calls)
			}
			// No messages read, embedding call, memory delete or insert is expected.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGlobalExtractionScopeAllowsRootsAndUnboundImports(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bound bool
	}{{"root conversation", true}, {"unbound import", false}} {
		t.Run(tc.name, func(t *testing.T) {
			extractor, mock, _ := branchTestExtractor(t)
			rows := scopeRows()
			if tc.bound {
				rows.AddRow(10, 1, 1, nil)
			} else {
				rows.AddRow(10, nil, nil, nil)
			}
			mock.ExpectQuery(scopeQuery).WithArgs(10, 1).WillReturnRows(rows)
			if err := extractor.checkGlobalExtraction(context.Background(), 10); err != nil {
				t.Fatalf("valid global scope rejected: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtractionScopeFailuresFailClosed(t *testing.T) {
	for _, name := range []string{"query error", "missing session", "dangling conversation"} {
		t.Run(name, func(t *testing.T) {
			extractor, mock, probe := branchTestExtractor(t)
			expect := mock.ExpectQuery(scopeQuery).WithArgs(10, 1)
			switch name {
			case "query error":
				expect.WillReturnError(errors.New("database unavailable"))
			case "missing session":
				expect.WillReturnRows(scopeRows())
			case "dangling conversation":
				expect.WillReturnRows(scopeRows().AddRow(10, 3, nil, nil))
			}
			if err := extractor.checkGlobalExtraction(context.Background(), 10); err == nil {
				t.Fatal("unknown scope permitted extraction")
			}
			if probe.calls != 0 {
				t.Fatal("scope failure called a model")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestScopeFailureDoesNotReachExtraction(t *testing.T) {
	extractor, mock, probe := branchTestExtractor(t)
	mock.ExpectQuery(scopeQuery).WithArgs(10, 1).WillReturnError(errors.New("database unavailable"))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "sessions" SET `).
		WithArgs(sqlmock.AnyArg(), store.ExtractFailed, sqlmock.AnyArg(), 10).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if count, err := extractor.ExtractNewToMemory(context.Background(), 10); count != 0 || err == nil || probe.calls != 0 {
		t.Fatalf("scope failure escaped: count=%d err=%v model_calls=%d", count, err, probe.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
