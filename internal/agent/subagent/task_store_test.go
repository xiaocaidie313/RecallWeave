package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"recallweave/internal/store"
	"strings"
	"testing"
	"time"
)

var taskStoreNow = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)

func taskStoreTestDB(t *testing.T) (*GormTaskStore, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	database, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return taskStoreNow }})
	if err != nil {
		t.Fatal(err)
	}
	return NewTaskStore(database), mock
}
func taskStoreRecord() store.SubagentTask {
	return store.SubagentTask{ID: "task-1", SubagentID: "child-1", ParentRunID: "parent-1", ConversationID: 9, AgentName: "researcher", Task: "Check original sources", Status: "queued"}
}
func taskStoreRows(records ...store.SubagentTask) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "subagent_id", "parent_run_id", "run_id", "previous_run_id", "conversation_id", "agent_name", "task", "status", "cancel_requested", "result", "error", "created_at", "updated_at", "started_at", "finished_at"})
	for _, r := range records {
		var raw any
		if len(r.Result) > 0 {
			raw = string(r.Result)
		}
		rows.AddRow(r.ID, r.SubagentID, r.ParentRunID, r.RunID, r.PreviousRunID, r.ConversationID, r.AgentName, r.Task, r.Status, r.CancelRequested, raw, r.Error, taskStoreNow, taskStoreNow, r.StartedAt, r.FinishedAt)
	}
	return rows
}

const taskLoadSQL = `SELECT .* FROM "subagent_tasks" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`
const taskLockSQL = taskLoadSQL + ` FOR UPDATE`

func taskRun(r store.SubagentTask) *store.AgentRun {
	return &store.AgentRun{RunID: r.RunID, ParentRunID: r.ParentRunID, SubagentID: r.SubagentID, PreviousRunID: r.PreviousRunID, ConversationID: r.ConversationID, Status: r.Status, State: json.RawMessage(`{"status":"completed"}`)}
}
func expectTaskFinish(mock sqlmock.Sqlmock, r store.SubagentTask, previous string) {
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = \$7 AND conversation_id = \$8 AND status = \$9`).WithArgs(r.RunID, r.Status, sessionJSONArgument(r.Result), r.Error, taskStoreNow, taskStoreNow, r.ID, r.ConversationID, previous).WillReturnResult(sqlmock.NewResult(0, 1))
}
func TestTaskStoreCreatePersistsQueuedTaskBeforeExecution(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "subagent_tasks" .*`).WithArgs(r.ID, r.SubagentID, r.ParentRunID, "", "", r.ConversationID, r.AgentName, r.Task, "queued", false, nil, "", taskStoreNow, taskStoreNow, nil, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repo.Create(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if !r.CreatedAt.Equal(taskStoreNow) || !r.UpdatedAt.Equal(taskStoreNow) {
		t.Fatalf("timestamps missing: %+v", r)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreLoadIsScopedAndRestoresJSON(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	r.Status = "completed"
	r.RunID = "child-run"
	r.Result = json.RawMessage(`{"content":"answer","state":{"run_id":"child-run"}}`)
	mock.ExpectQuery(taskLoadSQL).WithArgs(r.ID, r.ConversationID, 1).WillReturnRows(taskStoreRows(r))
	got, err := repo.Load(context.Background(), r.ID, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID || string(got.Result) != string(r.Result) {
		t.Fatalf("stored result lost: %+v", got)
	}
	mock.ExpectQuery(taskLoadSQL).WithArgs(r.ID, uint(100), 1).WillReturnRows(taskStoreRows())
	if _, err := repo.Load(context.Background(), r.ID, 100); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("foreign load returned %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreStartOnlyQueuedUncancelledTask(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = \$4 AND conversation_id = \$5 AND status = \$6 AND cancel_requested = \$7`).WithArgs(taskStoreNow, "running", taskStoreNow, r.ID, r.ConversationID, "queued", false).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	started, err := repo.Start(context.Background(), r.ID, r.ConversationID)
	if err != nil || !started {
		t.Fatalf("start=%v err=%v", started, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreFinishAtomicallySavesRunAndRetainsCancellation(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	original := taskStoreRecord()
	original.Status = "running"
	original.CancelRequested = true
	started := taskStoreNow.Add(-time.Minute)
	original.StartedAt = &started
	done := taskStoreRecord()
	done.Status = "completed"
	done.RunID = "child-run"
	done.Result = json.RawMessage(`{"content":"answer"}`)
	mock.ExpectBegin()
	mock.ExpectQuery(taskLockSQL).WithArgs(done.ID, done.ConversationID, 1).WillReturnRows(taskStoreRows(original))
	expectTaskFinish(mock, done, "running")
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(4))
	mock.ExpectCommit()
	if err := repo.Finish(context.Background(), &done, taskRun(done)); err != nil {
		t.Fatal(err)
	}
	if !done.CancelRequested || done.StartedAt == nil || !done.StartedAt.Equal(started) || done.FinishedAt == nil {
		t.Fatalf("durable task fields overwritten: %+v", done)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreFinishRunFailureRollsBackTaskResult(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	original := taskStoreRecord()
	original.Status = "running"
	done := original
	done.Status = "completed"
	done.RunID = "child-run"
	done.Result = json.RawMessage(`{"content":"answer"}`)
	failed := errors.New("database unavailable")
	mock.ExpectBegin()
	mock.ExpectQuery(taskLockSQL).WithArgs(done.ID, done.ConversationID, 1).WillReturnRows(taskStoreRows(original))
	expectTaskFinish(mock, done, "running")
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnError(failed)
	mock.ExpectRollback()
	if err := repo.Finish(context.Background(), &done, taskRun(done)); !errors.Is(err, failed) {
		t.Fatalf("want transactional failure, got %v", err)
	}
	if done.FinishedAt != nil || !done.UpdatedAt.IsZero() {
		t.Fatal("failed commit mutated task timestamps")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreFinishCannotOverwriteTerminalOrForeignTask(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "foreign"}[foreign], func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			r := taskStoreRecord()
			r.Status = "failed"
			r.Result = json.RawMessage(`{"error":"failed"}`)
			rows := taskStoreRows(r)
			if foreign {
				rows = taskStoreRows()
			}
			mock.ExpectBegin()
			mock.ExpectQuery(taskLockSQL).WithArgs(r.ID, r.ConversationID, 1).WillReturnRows(rows)
			mock.ExpectRollback()
			err := repo.Finish(context.Background(), &r, nil)
			want := ErrTaskConflict
			if foreign {
				want = ErrTaskNotFound
			}
			if !errors.Is(err, want) {
				t.Fatalf("want %v got %v", want, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTaskStoreQueuedCancellationPreventsExecution(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	mock.ExpectBegin()
	mock.ExpectQuery(taskLockSQL).WithArgs(r.ID, r.ConversationID, 1).WillReturnRows(taskStoreRows(r))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = \$5 AND conversation_id = \$6 AND status = \$7`).WithArgs("cancelled", true, taskStoreNow, taskStoreNow, r.ID, r.ConversationID, "queued").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := repo.RequestCancel(context.Background(), r.ID, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || !got.CancelRequested || got.FinishedAt == nil {
		t.Fatalf("queued cancellation lost: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreRunningCancellationIsRequestUntilWorkerEnds(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	r.Status = "running"
	mock.ExpectBegin()
	mock.ExpectQuery(taskLockSQL).WithArgs(r.ID, r.ConversationID, 1).WillReturnRows(taskStoreRows(r))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = \$3 AND conversation_id = \$4 AND status = \$5`).WithArgs(true, taskStoreNow, r.ID, r.ConversationID, "running").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := repo.RequestCancel(context.Background(), r.ID, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || !got.CancelRequested || got.FinishedAt != nil {
		t.Fatalf("running work marked finished too early: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreCancelTerminalIsIdempotent(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	r.Status = "completed"
	mock.ExpectBegin()
	mock.ExpectQuery(taskLockSQL).WithArgs(r.ID, r.ConversationID, 1).WillReturnRows(taskStoreRows(r))
	mock.ExpectCommit()
	got, err := repo.RequestCancel(context.Background(), r.ID, r.ConversationID)
	if err != nil || got.Status != "completed" || got.CancelRequested {
		t.Fatalf("terminal result changed: %+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreRestartInterruptsPendingWorkWithoutReplay(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE status IN \(\$5,\$6\)`).WithArgs("server stopped before task finished; automatic replay is disabled", taskStoreNow, "interrupted", taskStoreNow, "queued", "running").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	if err := repo.InterruptUnfinished(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStoreCheckpointBlocksStartedInterruptedContinuation(t *testing.T) {
	for _, count := range []int64{0, 1} {
		repo, mock := taskStoreTestDB(t)
		r := store.SubagentSession{ID: "child-1", ConversationID: 9, LatestRunID: "previous-run"}
		mock.ExpectQuery(`SELECT count\(\*\) FROM "subagent_tasks" WHERE subagent_id = \$1 AND conversation_id = \$2 AND previous_run_id = \$3 AND status = \$4 AND started_at IS NOT NULL`).WithArgs(r.ID, r.ConversationID, r.LatestRunID, "interrupted").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(count))
		err := repo.CheckTaskCheckpoint(context.Background(), r)
		if (count == 0 && err != nil) || (count == 1 && !errors.Is(err, ErrCheckpointUnavailable)) {
			t.Fatalf("count %d: %v", count, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestTaskStoreRejectsUnscopedMalformedAndCancelledOperations(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	for _, id := range []string{"", " task ", strings.Repeat("x", 65)} {
		if _, err := repo.Load(context.Background(), id, 9); err == nil {
			t.Fatal("invalid identity accepted")
		}
		if _, err := repo.RequestCancel(context.Background(), id, 9); err == nil {
			t.Fatal("invalid cancellation accepted")
		}
	}
	if _, err := repo.List(context.Background(), 0, 1); err == nil {
		t.Fatal("unscoped list accepted")
	}
	r := taskStoreRecord()
	r.Result = json.RawMessage(`{`)
	if err := repo.Create(context.Background(), &r); err == nil {
		t.Fatal("malformed result accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Start(ctx, "task-1", 9); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := NewTaskStore(nil).InterruptUnfinished(context.Background()); err == nil {
		t.Fatal("nil storage accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskStoreStartCannotRunCancelledOrForeignTask(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "foreign"}[foreign], func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			r := taskStoreRecord()
			r.Status = "cancelled"
			r.CancelRequested = true
			mock.ExpectBegin()
			mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = \$4 AND conversation_id = \$5 AND status = \$6 AND cancel_requested = \$7`).WithArgs(taskStoreNow, "running", taskStoreNow, r.ID, r.ConversationID, "queued", false).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectCommit()
			rows := taskStoreRows(r)
			if foreign {
				rows = taskStoreRows()
			}
			mock.ExpectQuery(taskLoadSQL).WithArgs(r.ID, r.ConversationID, 1).WillReturnRows(rows)
			started, err := repo.Start(context.Background(), r.ID, r.ConversationID)
			if started || (foreign && !errors.Is(err, ErrTaskNotFound)) || (!foreign && err != nil) {
				t.Fatalf("start=%v err=%v", started, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTaskStoreListIsScopedAndBounded(t *testing.T) {
	for _, check := range []struct{ requested, bounded int }{{0, 10}, {3, 3}, {1000, 50}} {
		repo, mock := taskStoreTestDB(t)
		r := taskStoreRecord()
		mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE conversation_id = \$1 ORDER BY created_at DESC,id DESC LIMIT \$2`).WithArgs(r.ConversationID, check.bounded).WillReturnRows(taskStoreRows(r))
		rows, err := repo.List(context.Background(), r.ConversationID, check.requested)
		if err != nil || len(rows) != 1 || rows[0].ConversationID != r.ConversationID {
			t.Fatalf("invalid scoped result: %+v %v", rows, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestTaskStoreRejectsMismatchedExecutionRunBeforeSQL(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	r := taskStoreRecord()
	r.Status = "completed"
	r.RunID = "child-run"
	run := taskRun(r)
	run.ConversationID = 100
	if err := repo.Finish(context.Background(), &r, run); err == nil {
		t.Fatal("foreign run accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
