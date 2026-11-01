package subagent

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

type publicTaskEventArgument struct{}

func (publicTaskEventArgument) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok || strings.Contains(raw, "SECRET_HISTORY") {
		return false
	}
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return false
	}
	return len(data) == 5 && data["conversation_id"] == float64(9) && data["task_id"] == "task-1" && data["subagent_id"] == "child-1" && data["run_id"] == "child-run" && data["status"] == "completed"
}

func expectTaskConversationLock(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).WithArgs(uint(9), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(9)))
}
func TestTaskStoreWithEventsCommitsTerminalInboxAndEventWithRun(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithEvents()
	original := taskStoreRecord()
	original.Status = "running"
	done := original
	done.Status = "completed"
	done.RunID = "child-run"
	done.Result = json.RawMessage(`{"state":{"context_snapshot":"SECRET_HISTORY"}}`)
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	mock.ExpectQuery(taskLockSQL).WithArgs(done.ID, done.ConversationID, 1).WillReturnRows(taskStoreRows(original))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.* WHERE id = .* AND conversation_id = .* AND status = .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(4))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WithArgs(uint(9), "task.finished", "task:task-1:finished", publicTaskEventArgument{}, taskStoreNow).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(8)))
	mock.ExpectCommit()
	if err := repo.Finish(context.Background(), &done, taskRun(done)); err != nil {
		t.Fatal(err)
	}
	if done.ReportStatus != "pending" {
		t.Fatalf("completion not scheduled for report: %+v", done)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreWithEventsRollsBackFinishWhenEventCannotPersist(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithEvents()
	original := taskStoreRecord()
	original.Status = "running"
	done := original
	done.Status = "failed"
	failed := errors.New("event storage unavailable")
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	mock.ExpectQuery(taskLockSQL).WithArgs(done.ID, done.ConversationID, 1).WillReturnRows(taskStoreRows(original))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnError(failed)
	mock.ExpectRollback()
	if err := repo.Finish(context.Background(), &done, nil); !errors.Is(err, failed) {
		t.Fatalf("nontransactional finish: %v", err)
	}
	if done.ReportStatus != "" || done.FinishedAt != nil {
		t.Fatal("failed transaction changed caller checkpoint")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreWithEventsQueuedCancellationSchedulesReport(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithEvents()
	row := taskStoreRecord()
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	mock.ExpectQuery(taskLockSQL).WithArgs(row.ID, row.ConversationID, 1).WillReturnRows(taskStoreRows(row))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(9)))
	mock.ExpectCommit()
	got, err := repo.RequestCancel(context.Background(), row.ID, row.ConversationID)
	if err != nil || got.Status != "cancelled" || got.ReportStatus != "pending" {
		t.Fatalf("queued cancellation: %+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreWithEventsRunningCancellationDoesNotAnnounceCompletion(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithEvents()
	row := taskStoreRecord()
	row.Status = "running"
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	mock.ExpectQuery(taskLockSQL).WithArgs(row.ID, row.ConversationID, 1).WillReturnRows(taskStoreRows(row))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*cancel_requested.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := repo.RequestCancel(context.Background(), row.ID, row.ConversationID)
	if err != nil || got.Status != "running" || !got.CancelRequested || got.ReportStatus != "" {
		t.Fatalf("running cancellation prematurely reported: %+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreWithEventsRecoveryPersistsInterruptedNotification(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithEvents()
	row := taskStoreRecord()
	row.Status = "running"
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE status IN .* ORDER BY id LIMIT`).WillReturnRows(taskStoreRows(row))
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	mock.ExpectQuery(taskLockSQL).WithArgs(row.ID, row.ConversationID, 1).WillReturnRows(taskStoreRows(row))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(10)))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE status IN .* ORDER BY id LIMIT`).WillReturnRows(taskStoreRows())
	if err := repo.InterruptUnfinished(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
