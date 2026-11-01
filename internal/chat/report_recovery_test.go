package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func interruptedReportRows(table string, mode any, status, reportStatus, claim string) *sqlmock.Rows {
	id := "task-1"
	if table == "task_groups" {
		id = "group-id"
	}
	return sqlmock.NewRows([]string{"id", "conversation_id", "delivery_mode", "status", "report_status", "report_claim", "report_run_id", "report_message_id"}).AddRow(id, uint(9), mode, status, reportStatus, claim, "", 0)
}

func TestReportRecoveryRetriesOnlyDeterministicDelivery(t *testing.T) {
	cases := []struct {
		name, table string
		mode        any
		retry       bool
	}{
		{"direct task", "subagent_tasks", "direct", true},
		{"legacy task", "subagent_tasks", "", true},
		{"null task", "subagent_tasks", nil, true},
		{"synthesized task", "subagent_tasks", "synthesize", false},
		{"invalid task", "subagent_tasks", "unexpected", false},
		{"combined group", "task_groups", "combined", true},
		{"legacy group", "task_groups", "", true},
		{"null group", "task_groups", nil, true},
		{"synthesized group", "task_groups", "synthesize", false},
		{"invalid group", "task_groups", "unexpected", false},
		{"direct group cannot have group report", "task_groups", "direct", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := reportTestDB(t)
			id, key := "task-1", "report:task-1:failed"
			if tc.table == "task_groups" {
				id, key = "group-id", "group-report:group-id:failed"
			}
			mock.ExpectQuery(`SELECT .* FROM "`+tc.table+`" WHERE report_status = \$1 ORDER BY id LIMIT \$2`).WithArgs("processing", 100).WillReturnRows(interruptedReportRows(tc.table, tc.mode, "completed", "processing", "old-claim"))
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).WithArgs(uint(9), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(9)))
			mock.ExpectQuery(`SELECT .* FROM "`+tc.table+`" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3 FOR UPDATE`).WithArgs(id, uint(9), 1).WillReturnRows(interruptedReportRows(tc.table, tc.mode, "completed", "processing", "old-claim"))
			status := "failed"
			var reason any = sqlmock.AnyArg()
			if tc.retry {
				status, reason = "pending", ""
			}
			mock.ExpectExec(`UPDATE "`+tc.table+`" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND report_claim = .*`).WithArgs("", reason, status, reportStoreNow, id, uint(9), "processing", "old-claim").WillReturnResult(sqlmock.NewResult(0, 1))
			if !tc.retry {
				mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WithArgs(uint(9), "report.failed", key, sqlmock.AnyArg(), reportStoreNow).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
			}
			mock.ExpectCommit()
			mock.ExpectQuery(`SELECT .* FROM "`+tc.table+`" WHERE report_status = \$1 AND id > \$2 ORDER BY id LIMIT \$3`).WithArgs("processing", id, 100).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			var err error
			if tc.table == "task_groups" {
				err = repo.InterruptGroupProcessing(context.Background())
			} else {
				err = repo.InterruptProcessing(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReportRecoveryDoesNotOverwriteDeliveredOrReclaimedRows(t *testing.T) {
	for _, table := range []string{"subagent_tasks", "task_groups"} {
		for _, changed := range []string{"delivered", "new claim", "already pending"} {
			t.Run(table+"/"+changed, func(t *testing.T) {
				repo, mock := reportTestDB(t)
				id, mode := "task-1", "direct"
				if table == "task_groups" {
					id, mode = "group-id", "combined"
				}
				status, claim := "delivered", ""
				if changed == "new claim" {
					status, claim = "processing", "new-claim"
				}
				if changed == "already pending" {
					status = "pending"
				}
				mock.ExpectQuery(`SELECT .* FROM "`+table+`" WHERE report_status = \$1 ORDER BY id LIMIT \$2`).WithArgs("processing", 100).WillReturnRows(interruptedReportRows(table, mode, "completed", "processing", "old-claim"))
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = .* FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
				mock.ExpectQuery(`SELECT .* FROM "` + table + `" WHERE id = .* AND conversation_id = .* FOR UPDATE`).WillReturnRows(interruptedReportRows(table, mode, "completed", status, claim))
				mock.ExpectCommit()
				// Keyset scanning visits the changed row once and cannot spin on a new claim.
				mock.ExpectQuery(`SELECT .* FROM "`+table+`" WHERE report_status = \$1 AND id > \$2 ORDER BY id LIMIT \$3`).WithArgs("processing", id, 100).WillReturnRows(sqlmock.NewRows([]string{"id"}))
				var err error
				if table == "task_groups" {
					err = repo.InterruptGroupProcessing(context.Background())
				} else {
					err = repo.InterruptProcessing(context.Background())
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReportRecoveryFailedEventRollsBackFailureState(t *testing.T) {
	for _, table := range []string{"subagent_tasks", "task_groups"} {
		t.Run(table, func(t *testing.T) {
			repo, mock := reportTestDB(t)
			id := "task-1"
			if table == "task_groups" {
				id = "group-id"
			}
			failed := errors.New("event storage unavailable")
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = .* FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
			mock.ExpectQuery(`SELECT .* FROM "` + table + `" WHERE id = .* AND conversation_id = .* FOR UPDATE`).WillReturnRows(interruptedReportRows(table, "synthesize", "completed", "processing", "claim-1"))
			mock.ExpectExec(`UPDATE "` + table + `" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnError(failed)
			mock.ExpectRollback()
			if err := repo.recoverInterruptedReport(context.Background(), table, id, 9, "claim-1"); !errors.Is(err, failed) {
				t.Fatalf("failure state was not atomic with failure event: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReportRecoveryNeverQueuesNonterminalExecution(t *testing.T) {
	for _, table := range []string{"subagent_tasks", "task_groups"} {
		t.Run(table, func(t *testing.T) {
			repo, mock := reportTestDB(t)
			id, mode := "task-1", "direct"
			if table == "task_groups" {
				id, mode = "group-id", "combined"
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = .* FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
			mock.ExpectQuery(`SELECT .* FROM "` + table + `" WHERE id = .* AND conversation_id = .* FOR UPDATE`).WillReturnRows(interruptedReportRows(table, mode, "running", "processing", "claim-1"))
			mock.ExpectExec(`UPDATE "`+table+`" SET .*`).WithArgs("", sqlmock.AnyArg(), "failed", reportStoreNow, id, uint(9), "processing", "claim-1").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
			mock.ExpectCommit()
			if err := repo.recoverInterruptedReport(context.Background(), table, id, 9, "claim-1"); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
