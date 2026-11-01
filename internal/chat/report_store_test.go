package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"recallweave/internal/store"
)

var reportStoreNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func reportTestDB(t *testing.T) (*GormReportStore, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return reportStoreNow }})
	if err != nil {
		t.Fatal(err)
	}
	return NewReportStore(db), mock
}
func reportTaskRows(status, claim string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "subagent_id", "parent_run_id", "run_id", "conversation_id", "agent_name", "task", "status", "report_status", "report_claim"}).AddRow("task-1", "child-1", "parent-1", "child-run", uint(9), "researcher", "check sources", "completed", status, claim)
}
func reportSessionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "conversation_id", "source_tag", "name"}).AddRow(uint(5), uint(9), store.SourceChat, "chat")
}
func expectReportLock(mock sqlmock.Sqlmock, reportStatus, claim string) {
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).WithArgs(uint(9), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(9)))
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3 FOR UPDATE`).WithArgs("task-1", uint(9), 1).WillReturnRows(reportTaskRows(reportStatus, claim))
}
func reportGenerated() GeneratedReport {
	return GeneratedReport{Message: store.Message{Role: "assistant", Content: "The research is finished; source is message 3."}, Run: &store.AgentRun{RunID: "report-run", ConversationID: 9, Status: "completed", Content: "summary", State: json.RawMessage(`{"status":"completed"}`)}}
}
func expectReportMessage(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE conversation_id = \$1 AND source_tag = \$2 .* LIMIT \$3`).WithArgs(uint(9), store.SourceChat, 1).WillReturnRows(reportSessionRows())
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE id = \$1 AND conversation_id = \$2 AND source_tag = \$3 LIMIT \$4 FOR UPDATE`).WithArgs(uint(5), uint(9), store.SourceChat, 1).WillReturnRows(reportSessionRows())
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(seq\), 0\) FROM "messages" WHERE session_id = \$1`).WithArgs(uint(5)).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(4))
	mock.ExpectQuery(`INSERT INTO "messages" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(6)))
}
func TestReportStoreClaimsOnlyPendingTaskInOwningConversation(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND status IN`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`).WithArgs("task-1", uint(9), 1).WillReturnRows(reportTaskRows("processing", "claim-1"))
	mock.ExpectCommit()
	row, claimed, err := repo.Claim(context.Background(), "task-1", 9, "claim-1")
	if err != nil || !claimed || row.ReportClaim != "claim-1" {
		t.Fatalf("claim=%v row=%+v err=%v", claimed, row, err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND status IN`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`).WithArgs("task-1", uint(99), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()
	if _, claimed, err := repo.Claim(context.Background(), "task-1", 99, "foreign"); !errors.Is(err, gorm.ErrRecordNotFound) || claimed {
		t.Fatalf("foreign claim: %v %v", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreDeliveryCommitsMessageRunAndEventTogether(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectReportLock(mock, "processing", "claim-1")
	expectReportMessage(mock)
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(7)))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND report_claim = .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(10)))
	mock.ExpectCommit()
	if err := repo.Deliver(context.Background(), "task-1", 9, "claim-1", reportGenerated()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreDuplicateDeliveryDoesNotAppendAnotherMessage(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectReportLock(mock, "delivered", "")
	mock.ExpectCommit()
	if err := repo.Deliver(context.Background(), "task-1", 9, "claim-1", reportGenerated()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreRejectsWrongClaimBeforeWritingMessage(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectReportLock(mock, "processing", "other-claim")
	mock.ExpectRollback()
	if err := repo.Deliver(context.Background(), "task-1", 9, "claim-1", reportGenerated()); !errors.Is(err, ErrReportConflict) {
		t.Fatalf("wrong claim accepted: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreEventFailureRollsBackMessageAndRun(t *testing.T) {
	repo, mock := reportTestDB(t)
	failed := errors.New("event insert unavailable")
	mock.ExpectBegin()
	expectReportLock(mock, "processing", "claim-1")
	expectReportMessage(mock)
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(7)))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND report_claim = .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnError(failed)
	mock.ExpectRollback()
	if err := repo.Deliver(context.Background(), "task-1", 9, "claim-1", reportGenerated()); !errors.Is(err, failed) {
		t.Fatalf("event failure not transactional: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreFailurePersistsIndependentRunAndFailureEvent(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectReportLock(mock, "processing", "claim-1")
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(7)))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND report_claim = .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(11)))
	mock.ExpectCommit()
	if err := repo.Fail(context.Background(), "task-1", 9, "claim-1", "model timed out", reportGenerated().Run); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreRecoveryFailsInterruptedGenerationWithoutReplaying(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE report_status = \$1 ORDER BY id LIMIT \$2`).WithArgs("processing", 100).WillReturnRows(reportTaskRows("processing", "old-process"))
	mock.ExpectBegin()
	expectReportLock(mock, "processing", "old-process")
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(12)))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE report_status = \$1 ORDER BY id LIMIT \$2`).WithArgs("processing", 100).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if err := repo.InterruptProcessing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStorePendingUsesOnlyExplicitTerminalInbox(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectQuery(`SELECT "id","conversation_id" FROM "subagent_tasks" WHERE report_status = .* AND status IN .* ORDER BY finished_at ASC,id ASC LIMIT`).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id"}).AddRow("task-1", uint(9)))
	rows, err := repo.Pending(context.Background(), 200)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending=%+v err=%v", rows, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreOrdinaryTurnAppendsOrderedMessagesAndEvents(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE id = \$1 LIMIT \$2`).WithArgs(uint(5), 1).WillReturnRows(reportSessionRows())
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).WithArgs(uint(9), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(9)))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE id = \$1 AND conversation_id = \$2 AND source_tag = \$3 LIMIT \$4 FOR UPDATE`).WithArgs(uint(5), uint(9), store.SourceChat, 1).WillReturnRows(reportSessionRows())
	for index := 0; index < 2; index++ {
		mock.ExpectQuery(`SELECT COALESCE\(MAX\(seq\), 0\) FROM "messages" WHERE session_id = \$1`).WithArgs(uint(5)).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(index + 4))
		mock.ExpectQuery(`INSERT INTO "messages" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(index + 6))
		mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(index + 10))
	}
	mock.ExpectCommit()
	if err := repo.AppendTurn(context.Background(), 5, "question", "answer"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestReportStoreRejectsForeignRunAndCancelledContextWithoutSQL(t *testing.T) {
	repo, mock := reportTestDB(t)
	report := reportGenerated()
	report.Run.ConversationID = 99
	if err := repo.Deliver(context.Background(), "task-1", 9, "claim-1", report); err == nil {
		t.Fatal("foreign run accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Pending(ctx, 20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := repo.AppendTurn(context.Background(), 0, "q", "a"); err == nil {
		t.Fatal("unowned turn accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
