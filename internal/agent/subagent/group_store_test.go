package subagent

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
)

func groupStoreFixture() (store.TaskGroup, []store.SubagentTask) {
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	raw, _ := json.Marshal(ids)
	group := store.TaskGroup{ID: strings.Repeat("f", 32), ConversationID: 9, ParentRunID: "parent-1", Goal: "Compare original history and external sources", DeliveryMode: "combined", TaskIDs: raw, Status: "running"}
	members := make([]store.SubagentTask, 2)
	for i, id := range ids {
		members[i] = store.SubagentTask{ID: id, GroupID: group.ID, SubagentID: strings.Repeat(string(rune('c'+i)), 32), ParentRunID: group.ParentRunID, ConversationID: 9, AgentName: "researcher", Task: "independent research", Status: "queued", ReportStatus: "grouped"}
	}
	return group, members
}

func groupStoreRows(groups ...store.TaskGroup) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "conversation_id", "parent_run_id", "goal", "expected_output", "delivery_mode", "task_ids", "status", "cancel_requested", "report_status", "report_claim", "report_run_id", "report_message_id", "report_error", "reported_at", "created_at", "updated_at", "finished_at"})
	for _, g := range groups {
		rows.AddRow(g.ID, g.ConversationID, g.ParentRunID, g.Goal, g.ExpectedOutput, g.DeliveryMode, string(g.TaskIDs), g.Status, g.CancelRequested, g.ReportStatus, g.ReportClaim, g.ReportRunID, g.ReportMessageID, g.ReportError, g.ReportedAt, taskStoreNow, taskStoreNow, g.FinishedAt)
	}
	return rows
}
func groupMemberRows(members ...store.SubagentTask) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "group_id", "subagent_id", "parent_run_id", "run_id", "previous_run_id", "conversation_id", "agent_name", "task", "delivery_mode", "status", "cancel_requested", "result", "error", "created_at", "updated_at", "started_at", "finished_at", "report_status"})
	for _, r := range members {
		var raw any
		if len(r.Result) > 0 {
			raw = string(r.Result)
		}
		rows.AddRow(r.ID, r.GroupID, r.SubagentID, r.ParentRunID, r.RunID, r.PreviousRunID, r.ConversationID, r.AgentName, r.Task, r.DeliveryMode, r.Status, r.CancelRequested, raw, r.Error, taskStoreNow, taskStoreNow, r.StartedAt, r.FinishedAt, r.ReportStatus)
	}
	return rows
}

const groupLockSQL = `SELECT .* FROM "task_groups" WHERE id = \$1 AND conversation_id = \$2 LIMIT \$3 FOR UPDATE`
const groupMembersSQL = `SELECT .* FROM "subagent_tasks" WHERE group_id = .* AND conversation_id = .* AND id IN .* ORDER BY id ASC`

type groupEventArg struct {
	kind, status string
	groupID      string
}

func (w groupEventArg) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok || strings.Contains(raw, "PRIVATE_SNAPSHOT") {
		return false
	}
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return false
	}
	return data["conversation_id"] == float64(9) && data["group_id"] == w.groupID && data["status"] == w.status
}
func expectGroupLock(mock sqlmock.Sqlmock, group store.TaskGroup) {
	mock.ExpectQuery(groupLockSQL).WithArgs(group.ID, group.ConversationID, 1).WillReturnRows(groupStoreRows(group))
}
func expectGroupEvent(mock sqlmock.Sqlmock, group store.TaskGroup, status string) {
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WithArgs(uint(9), "task_group.finished", "group:"+group.ID+":finished", groupEventArg{status: status, groupID: group.ID}, taskStoreNow).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(90))
}

func TestGroupStoreCreatesTwoMembersAtomicallyAndRollsBackPartialInsert(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups()
			group, members := groupStoreFixture()
			mock.ExpectBegin()
			expectTaskConversationLock(mock)
			mock.ExpectExec(`INSERT INTO "task_groups" .*`).WillReturnResult(sqlmock.NewResult(0, 1))
			if fail {
				mock.ExpectExec(`INSERT INTO "subagent_tasks" .*`).WillReturnError(errors.New("member insert failed"))
				mock.ExpectRollback()
			} else {
				mock.ExpectExec(`INSERT INTO "subagent_tasks" .*`).WillReturnResult(sqlmock.NewResult(0, 2))
				mock.ExpectCommit()
			}
			err := repo.CreateGroup(context.Background(), &group, members)
			if fail && err == nil || !fail && err != nil {
				t.Fatalf("atomic create failure=%v err=%v", fail, err)
			}
			if fail && !group.CreatedAt.IsZero() {
				t.Fatal("failed create mutated caller")
			}
			if !fail && (!group.CreatedAt.Equal(taskStoreNow) || members[0].ReportStatus != "grouped") {
				t.Fatalf("create fields missing: %+v", group)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupStoreRejectsUnsealedOrForeignMembershipBeforeSQL(t *testing.T) {
	for _, kind := range []string{"duplicate", "wrong_owner", "third_member", "standalone", "same_context", "nonqueued"} {
		t.Run(kind, func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups()
			g, m := groupStoreFixture()
			switch kind {
			case "duplicate":
				g.TaskIDs, _ = json.Marshal([]string{m[0].ID, m[0].ID})
			case "wrong_owner":
				m[1].ConversationID = 10
			case "third_member":
				m = append(m, m[0])
			case "standalone":
				m[1].GroupID = ""
			case "same_context":
				m[1].SubagentID = m[0].SubagentID
			case "nonqueued":
				m[1].Status = "completed"
			}
			if err := repo.CreateGroup(context.Background(), &g, m); err == nil {
				t.Fatal("invalid group accepted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupStoreFirstMemberFinishesWithoutSchedulingItsOwnOrGroupReport(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	members[0].Status = "running"
	done := members[0]
	done.Status = "completed"
	done.RunID = "child-run"
	done.Result = json.RawMessage(`{"content":"answer","state":{"context_snapshot":"PRIVATE_SNAPSHOT"}}`)
	current := done
	other := members[1]
	other.Status = "running"
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	expectGroupLock(mock, group)
	mock.ExpectQuery(taskLockSQL).WithArgs(done.ID, uint(9), 1).WillReturnRows(groupMemberRows(members[0]))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "agent_runs" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WithArgs(uint(9), "task.finished", "task:"+done.ID+":finished", groupEventArg{status: "completed", groupID: group.ID}, taskStoreNow).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(4))
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(current, other))
	mock.ExpectCommit()
	if err := repo.Finish(context.Background(), &done, taskRun(done)); err != nil {
		t.Fatal(err)
	}
	if done.ReportStatus != "grouped" {
		t.Fatalf("member scheduled a duplicate standalone report: %+v", done)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreLastMemberAggregatesOneTerminalGroupEvent(t *testing.T) {
	for _, test := range []struct {
		first, last, want string
		cancel            bool
	}{{"completed", "completed", "completed", false}, {"failed", "completed", "partial", false}, {"completed", "cancelled", "partial", false}, {"failed", "failed", "failed", false}, {"cancelled", "cancelled", "failed", false}, {"cancelled", "cancelled", "cancelled", true}, {"interrupted", "interrupted", "interrupted", false}} {
		t.Run(test.first+"_"+test.last+"_"+test.want, func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups().WithEvents()
			group, members := groupStoreFixture()
			group.CancelRequested = test.cancel
			members[0].Status = test.first
			members[1].Status = "running"
			done := members[1]
			done.Status = test.last
			mock.ExpectBegin()
			expectTaskConversationLock(mock)
			expectGroupLock(mock, group)
			mock.ExpectQuery(taskLockSQL).WithArgs(done.ID, uint(9), 1).WillReturnRows(groupMemberRows(members[1]))
			mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
			mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members[0], done))
			mock.ExpectExec(`UPDATE "task_groups" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
			expectGroupEvent(mock, group, test.want)
			mock.ExpectCommit()
			if err := repo.Finish(context.Background(), &done, nil); err != nil {
				t.Fatal(err)
			}
			if done.ReportStatus != "grouped" {
				t.Fatal("member report not suppressed")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupStoreTerminalEventFailureRollsBackLastMemberAndGroup(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	members[0].Status = "completed"
	members[1].Status = "running"
	done := members[1]
	done.Status = "completed"
	failure := errors.New("group event failed")
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	expectGroupLock(mock, group)
	mock.ExpectQuery(taskLockSQL).WillReturnRows(groupMemberRows(members[1]))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(8))
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members[0], done))
	mock.ExpectExec(`UPDATE "task_groups" SET .*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnError(failure)
	mock.ExpectRollback()
	if err := repo.Finish(context.Background(), &done, nil); !errors.Is(err, failure) {
		t.Fatalf("group update not atomic: %v", err)
	}
	if done.FinishedAt != nil {
		t.Fatal("rolled-back finish mutated caller")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreCancellationWaitsForRunningMemberToExit(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	members[1].Status = "running"
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	expectGroupLock(mock, group)
	mock.ExpectQuery(groupMembersSQL + ` FOR UPDATE`).WillReturnRows(groupMemberRows(members...))
	mock.ExpectExec(`UPDATE "task_groups" SET .*cancel_requested.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(10))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*cancel_requested.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	after := append([]store.SubagentTask(nil), members...)
	after[0].Status = "cancelled"
	after[0].CancelRequested = true
	after[1].CancelRequested = true
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(after...))
	mock.ExpectCommit()
	got, children, err := repo.RequestCancelGroup(context.Background(), group.ID, 9)
	if err != nil || got.Status != "running" || !got.CancelRequested || children[0].Status != "cancelled" || children[1].Status != "running" || !children[1].CancelRequested {
		t.Fatalf("cancel prematurely finalized group: %+v %+v %v", got, children, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreCancellingAllQueuedMembersProducesOneGroupReport(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	expectGroupLock(mock, group)
	mock.ExpectQuery(groupMembersSQL + ` FOR UPDATE`).WillReturnRows(groupMemberRows(members...))
	mock.ExpectExec(`UPDATE "task_groups" SET .*cancel_requested.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	for range members {
		mock.ExpectExec(`UPDATE "subagent_tasks" SET .*status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(11))
	}
	for i := range members {
		members[i].Status = "cancelled"
		members[i].CancelRequested = true
	}
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members...))
	mock.ExpectExec(`UPDATE "task_groups" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectGroupEvent(mock, group, "cancelled")
	mock.ExpectCommit()
	got, _, err := repo.RequestCancelGroup(context.Background(), group.ID, 9)
	if err != nil || got.Status != "cancelled" || got.ReportStatus != "pending" {
		t.Fatalf("all queued cancel did not finalize: %+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreRecoveryFinishesInterruptedGroupWithoutReplaying(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	members[0].Status = "completed"
	members[1].Status = "running"
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE status IN .* ORDER BY id LIMIT`).WillReturnRows(groupMemberRows(members[1]))
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	expectGroupLock(mock, group)
	mock.ExpectQuery(taskLockSQL).WillReturnRows(groupMemberRows(members[1]))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12))
	members[1].Status = "interrupted"
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members...))
	mock.ExpectExec(`UPDATE "task_groups" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectGroupEvent(mock, group, "partial")
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE status IN .* ORDER BY id LIMIT`).WillReturnRows(groupMemberRows())
	if err := repo.InterruptUnfinished(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreLoadRejectsForeignOwnerAndPreservesMemberOrder(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups()
	group, members := groupStoreFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE id = .* AND conversation_id = .* LIMIT .* FOR SHARE`).WithArgs(group.ID, uint(100), 1).WillReturnRows(groupStoreRows())
	mock.ExpectRollback()
	if _, _, err := repo.LoadGroup(context.Background(), group.ID, 100); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("foreign group loaded: %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE id = .* AND conversation_id = .* LIMIT .* FOR SHARE`).WithArgs(group.ID, uint(9), 1).WillReturnRows(groupStoreRows(group))
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members[1], members[0]))
	mock.ExpectCommit()
	_, got, err := repo.LoadGroup(context.Background(), group.ID, 9)
	if err != nil || len(got) != 2 || got[0].ID != members[0].ID {
		t.Fatalf("membership lost: %+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreIndividualQueuedCancellationUpdatesGroupInsteadOfStandaloneReport(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	members[0].Status = "completed"
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE id = .* AND conversation_id = .* LIMIT`).WithArgs(members[1].ID, uint(9), 1).WillReturnRows(sqlmock.NewRows([]string{"id", "group_id"}).AddRow(members[1].ID, group.ID))
	expectGroupLock(mock, group)
	mock.ExpectQuery(taskLockSQL).WithArgs(members[1].ID, uint(9), 1).WillReturnRows(groupMemberRows(members[1]))
	mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(15))
	members[1].Status = "cancelled"
	members[1].CancelRequested = true
	mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members...))
	mock.ExpectExec(`UPDATE "task_groups" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectGroupEvent(mock, group, "partial")
	mock.ExpectCommit()
	row, err := repo.RequestCancel(context.Background(), members[1].ID, 9)
	if err != nil || row.ReportStatus != "grouped" || row.Status != "cancelled" {
		t.Fatalf("individual group cancellation: %+v %v", row, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreRepeatedTerminalCancellationDoesNotScheduleAnotherReport(t *testing.T) {
	repo, mock := taskStoreTestDB(t)
	repo.WithTaskGroups().WithEvents()
	group, members := groupStoreFixture()
	group.Status = "completed"
	group.ReportStatus = "delivered"
	members[0].Status = "completed"
	members[1].Status = "completed"
	mock.ExpectBegin()
	expectTaskConversationLock(mock)
	expectGroupLock(mock, group)
	mock.ExpectQuery(groupMembersSQL + ` FOR UPDATE`).WillReturnRows(groupMemberRows(members...))
	mock.ExpectCommit()
	got, _, err := repo.RequestCancelGroup(context.Background(), group.ID, 9)
	if err != nil || got.ReportStatus != "delivered" || got.Status != "completed" || got.CancelRequested {
		t.Fatalf("terminal group cancellation changed outcome: %+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupStoreCreatesDeliveryModeAndMatchingMemberQueue(t *testing.T) {
	for _, mode := range []string{"", "direct", "combined", "synthesize"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups().WithEvents()
			group, members := groupStoreFixture()
			group.DeliveryMode = mode
			for i := range members {
				members[i].ReportStatus = ""
			}
			mock.ExpectBegin()
			expectTaskConversationLock(mock)
			mock.ExpectExec(`INSERT INTO "task_groups" .*`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO "subagent_tasks" .*`).WillReturnResult(sqlmock.NewResult(0, 2))
			mock.ExpectCommit()
			if err := repo.CreateGroup(context.Background(), &group, members); err != nil {
				t.Fatal(err)
			}
			wantMode, wantGroupReport, wantMemberReport := mode, "", "grouped"
			if mode == "" || mode == "direct" {
				wantMode, wantGroupReport, wantMemberReport = "direct", "not_required", ""
			}
			if group.DeliveryMode != wantMode || group.ReportStatus != wantGroupReport {
				t.Fatalf("wrong persisted group policy: %+v", group)
			}
			for _, member := range members {
				if member.DeliveryMode != "direct" || member.ReportStatus != wantMemberReport {
					t.Fatalf("wrong member queue: %+v", member)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupStoreFinishingUsesSavedModeAndLegacyBlankRemainsGrouped(t *testing.T) {
	for _, mode := range []string{"direct", "combined", "synthesize", ""} {
		for _, last := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "_first", true: "_last"}[last], func(t *testing.T) {
				repo, mock := taskStoreTestDB(t)
				repo.WithTaskGroups().WithEvents()
				group, members := groupStoreFixture()
				group.DeliveryMode = mode
				wantMemberReport, wantGroupReport := "grouped", "pending"
				if mode == "direct" {
					group.ReportStatus = "not_required"
					wantMemberReport, wantGroupReport = "pending", "not_required"
				}
				members[0].Status = "running"
				members[1].Status = "running"
				if last {
					members[1].Status = "completed"
				}
				done := members[0]
				done.Status = "completed"
				mock.ExpectBegin()
				expectTaskConversationLock(mock)
				expectGroupLock(mock, group)
				mock.ExpectQuery(taskLockSQL).WillReturnRows(groupMemberRows(members[0]))
				mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
				mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(done, members[1]))
				if last {
					mock.ExpectExec(`UPDATE "task_groups" SET .*report_status.*`).WithArgs("completed", wantGroupReport, taskStoreNow, taskStoreNow, group.ID, uint(9), "running").WillReturnResult(sqlmock.NewResult(0, 1))
					expectGroupEvent(mock, group, "completed")
				}
				mock.ExpectCommit()
				if err := repo.Finish(context.Background(), &done, nil); err != nil {
					t.Fatal(err)
				}
				if done.ReportStatus != wantMemberReport || group.DeliveryMode != mode {
					t.Fatalf("delivery shape changed: %+v group=%+v", done, group)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestGroupStoreCancellationKeepsSavedDeliveryPolicy(t *testing.T) {
	for _, mode := range []string{"direct", "combined", "synthesize", ""} {
		t.Run("mode_"+mode, func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups().WithEvents()
			group, members := groupStoreFixture()
			group.DeliveryMode = mode
			wantMemberReport, wantGroupReport := "grouped", "pending"
			if mode == "direct" {
				group.ReportStatus = "not_required"
				wantMemberReport, wantGroupReport = "pending", "not_required"
			}
			mock.ExpectBegin()
			expectTaskConversationLock(mock)
			expectGroupLock(mock, group)
			mock.ExpectQuery(groupMembersSQL + ` FOR UPDATE`).WillReturnRows(groupMemberRows(members...))
			mock.ExpectExec(`UPDATE "task_groups" SET .*cancel_requested.*`).WillReturnResult(sqlmock.NewResult(0, 1))
			for i := range members {
				mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(i + 1))
				members[i].Status, members[i].CancelRequested = "cancelled", true
			}
			mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members...))
			mock.ExpectExec(`UPDATE "task_groups" SET .*report_status.*`).WillReturnResult(sqlmock.NewResult(0, 1))
			expectGroupEvent(mock, group, "cancelled")
			mock.ExpectCommit()
			got, tasks, err := repo.RequestCancelGroup(context.Background(), group.ID, 9)
			if err != nil || got.DeliveryMode != mode || got.ReportStatus != wantGroupReport || tasks[0].ReportStatus != wantMemberReport || tasks[1].ReportStatus != wantMemberReport {
				t.Fatalf("cancel changed delivery policy: %+v tasks=%+v err=%v", got, tasks, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupStoreRecoveryUsesSavedDeliveryPolicy(t *testing.T) {
	for _, mode := range []string{"direct", "combined", "synthesize", ""} {
		t.Run("mode_"+mode, func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups().WithEvents()
			group, members := groupStoreFixture()
			group.DeliveryMode = mode
			members[0].Status, members[1].Status = "running", "running"
			wantMemberReport := "grouped"
			if mode == "direct" {
				group.ReportStatus, wantMemberReport = "not_required", "pending"
			}
			mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE status IN .* ORDER BY id LIMIT`).WillReturnRows(groupMemberRows(members[0]))
			mock.ExpectBegin()
			expectTaskConversationLock(mock)
			expectGroupLock(mock, group)
			mock.ExpectQuery(taskLockSQL).WillReturnRows(groupMemberRows(members[0]))
			mock.ExpectExec(`UPDATE "subagent_tasks" SET .*report_status.*`).WithArgs("interrupted", "server stopped before task finished; automatic replay is disabled", taskStoreNow, taskStoreNow, wantMemberReport, members[0].ID, uint(9)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`INSERT INTO "conversation_events" .* RETURNING "id"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
			members[0].Status = "interrupted"
			mock.ExpectQuery(groupMembersSQL).WillReturnRows(groupMemberRows(members...))
			mock.ExpectCommit()
			mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE status IN .* ORDER BY id LIMIT`).WillReturnRows(groupMemberRows())
			if err := repo.InterruptUnfinished(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupStoreRejectsContradictoryNewDeliveryShapeBeforeSQL(t *testing.T) {
	for _, kind := range []string{"unknown_mode", "direct_grouped_member", "combined_unnecessary", "synthesized_member"} {
		t.Run(kind, func(t *testing.T) {
			repo, mock := taskStoreTestDB(t)
			repo.WithTaskGroups()
			group, members := groupStoreFixture()
			switch kind {
			case "unknown_mode":
				group.DeliveryMode = "smart"
			case "direct_grouped_member":
				group.DeliveryMode = "direct"
			case "combined_unnecessary":
				group.ReportStatus = "not_required"
			case "synthesized_member":
				members[0].DeliveryMode = "synthesize"
			}
			if err := repo.CreateGroup(context.Background(), &group, members); err == nil {
				t.Fatal("contradictory delivery shape accepted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
