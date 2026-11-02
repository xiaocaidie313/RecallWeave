package chat

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/gorm"
	"recallweave/internal/agent/runtime"
)

func reportGroupRows(reportStatus, claim string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "conversation_id", "parent_run_id", "goal", "expected_output", "task_ids", "status", "report_status", "report_claim", "delivery_mode"}).AddRow("group-id", uint(9), "original-parent", "compare personal and external evidence", "cite sources and show contradictions", `["task-a","task-b"]`, "partial", reportStatus, claim, "synthesize")
}
func reportGroupTaskRows(t *testing.T) *sqlmock.Rows {
	_, tasks := groupReportFixture(t)
	rows := sqlmock.NewRows([]string{"id", "group_id", "conversation_id", "parent_run_id", "subagent_id", "run_id", "agent_name", "task", "status", "error", "result"})
	for _, task := range tasks {
		var result any
		if len(task.Result) > 0 {
			result = string(task.Result)
		}
		rows.AddRow(task.ID, task.GroupID, task.ConversationID, task.ParentRunID, task.SubagentID, task.RunID, task.AgentName, task.Task, task.Status, task.Error, result)
	}
	return rows
}
func expectGroupReportLock(mock sqlmock.Sqlmock, status, claim string) {
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).WithArgs(uint(9), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(9)))
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3 FOR UPDATE`).WithArgs("group-id", uint(9), 1).WillReturnRows(reportGroupRows(status, claim))
}

type groupMessageEventArgument struct{}

func (groupMessageEventArgument) Match(value driver.Value) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	var data map[string]any
	if json.Unmarshal([]byte(text), &data) != nil {
		return false
	}
	_, taskPresent := data["task_id"]
	message, messagePresent := data["message"].(map[string]any)
	return data["group_id"] == "group-id" && data["run_id"] == "report-run" && !taskPresent && messagePresent && message["id"] == float64(6)
}

func TestGroupReportStoreClaimsClosedGroupAndExactSavedMembers(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "task_groups" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND status IN`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`).WithArgs("group-id", uint(9), 1).WillReturnRows(reportGroupRows("processing", "claim-1"))
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE id IN \(\$1,\$2\) AND group_id = \$3 AND conversation_id = \$4`).WithArgs("task-a", "task-b", "group-id", uint(9)).WillReturnRows(reportGroupTaskRows(t))
	mock.ExpectCommit()
	group, tasks, claimed, err := repo.ClaimGroup(context.Background(), "group-id", 9, "claim-1")
	if err != nil || !claimed || group.ReportClaim != "claim-1" || len(tasks) != 2 {
		t.Fatalf("group claim failed: %+v tasks=%d claimed=%v err=%v", group, len(tasks), claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreDoesNotClaimAnotherConversation(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "task_groups" SET .*`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3`).WithArgs("group-id", uint(99), 1).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()
	if _, _, claimed, err := repo.ClaimGroup(context.Background(), "group-id", 99, "claim-1"); !errors.Is(err, gorm.ErrRecordNotFound) || claimed {
		t.Fatalf("foreign group claimed: %v %v", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreDeliveryCommitsOneMessageRunAndGroupEvent(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectGroupReportLock(mock, "processing", "claim-1")
	expectReportMessage(mock)
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(7)))
	mock.ExpectExec(`UPDATE "task_groups" SET .* WHERE id = .* AND conversation_id = .* AND report_status = .* AND report_claim = .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WithArgs(uint(9), "message.created", "message:6", groupMessageEventArgument{}, reportStoreNow).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(10)))
	mock.ExpectCommit()
	if err := repo.DeliverGroup(context.Background(), "group-id", 9, "claim-1", reportGenerated()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreRepeatDeliveryIsIdempotent(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectGroupReportLock(mock, "delivered", "")
	mock.ExpectCommit()
	if err := repo.DeliverGroup(context.Background(), "group-id", 9, "old-claim", reportGenerated()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreWrongClaimDoesNotWriteMessage(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectGroupReportLock(mock, "processing", "other-claim")
	mock.ExpectRollback()
	if err := repo.DeliverGroup(context.Background(), "group-id", 9, "claim-1", reportGenerated()); !errors.Is(err, ErrReportConflict) {
		t.Fatalf("wrong claim accepted: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreEventFailureRollsBackGroupMessageAndRun(t *testing.T) {
	repo, mock := reportTestDB(t)
	failed := errors.New("event unavailable")
	mock.ExpectBegin()
	expectGroupReportLock(mock, "processing", "claim-1")
	expectReportMessage(mock)
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(7)))
	mock.ExpectExec(`UPDATE "task_groups" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnError(failed)
	mock.ExpectRollback()
	if err := repo.DeliverGroup(context.Background(), "group-id", 9, "claim-1", reportGenerated()); !errors.Is(err, failed) {
		t.Fatalf("event failure ignored: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreFailurePreservesMainRunAndFailureEvent(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	expectGroupReportLock(mock, "processing", "claim-1")
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint(7)))
	mock.ExpectExec(`UPDATE "task_groups" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(10)))
	mock.ExpectCommit()
	if err := repo.FailGroup(context.Background(), "group-id", 9, "claim-1", "model interrupted", reportGenerated().Run); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreRecoveryDoesNotReplayGeneratingModel(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE report_status = \$1 ORDER BY id LIMIT \$2`).WithArgs("processing", 100).WillReturnRows(reportGroupRows("processing", "old-claim"))
	mock.ExpectBegin()
	expectGroupReportLock(mock, "processing", "old-claim")
	mock.ExpectExec(`UPDATE "task_groups" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uint64(10)))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE report_status = \$1 AND id > \$2 ORDER BY id LIMIT \$3`).WithArgs("processing", "group-id", 100).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if err := repo.InterruptGroupProcessing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStorePendingReadsOnlyMetadataAndTerminalInbox(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectQuery(`SELECT "id","conversation_id","delivery_mode" FROM "task_groups" WHERE report_status = .* AND status IN .* AND COALESCE\(delivery_mode, ''\) <> .* ORDER BY finished_at ASC,id ASC LIMIT`).WithArgs("pending", "completed", "partial", "failed", "cancelled", "interrupted", "direct", 20).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id"}).AddRow("group-id", uint(9)))
	rows, err := repo.PendingGroups(context.Background(), 200)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending groups=%+v err=%v", rows, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreDoesNotClaimDirectGroupEvenWithBadPendingStatus(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "task_groups" SET .* AND COALESCE\(delivery_mode, ''\) <> .*`).WithArgs("claim-1", "", "processing", reportStoreNow, "group-id", uint(9), "pending", "completed", "partial", "failed", "cancelled", "interrupted", "direct").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE id = .* AND conversation_id = .* LIMIT`).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "delivery_mode", "report_status"}).AddRow("group-id", uint(9), "direct", "pending"))
	mock.ExpectCommit()
	group, members, claimed, err := repo.ClaimGroup(context.Background(), "group-id", 9, "claim-1")
	if err != nil || claimed || len(members) != 0 || group.DeliveryMode != "direct" {
		t.Fatalf("direct group claimed for final report: %+v claimed=%v err=%v", group, claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupReportStoreCombinedPreservesSourcesWithoutNewRun(t *testing.T) {
	repo, mock := reportTestDB(t)
	report := reportGenerated()
	report.Run = nil
	report.Sources = []runtime.Source{{SourceRunID: "run-a", Kind: "memory", Tool: "get_messages", SessionID: 5, SeqStart: 1, SeqEnd: 2}, {SourceRunID: "run-b", Kind: "web", Tool: "search", URL: "https://example.com/evidence"}}
	mock.ExpectBegin()
	expectGroupReportLock(mock, "processing", "claim-1")
	expectReportMessage(mock)
	mock.ExpectExec(`UPDATE "task_groups" SET .*`).WithArgs("", "", uint(6), "", "delivered", reportStoreNow, reportStoreNow, "group-id", uint(9), "processing", "claim-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WithArgs(uint(9), "message.created", "message:6", deliveryMessageEventArgument{groupID: "group-id", sources: report.Sources}, reportStoreNow).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(10))
	mock.ExpectCommit()
	if err := repo.DeliverGroup(context.Background(), "group-id", 9, "claim-1", report); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
