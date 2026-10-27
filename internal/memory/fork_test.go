package memory

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/gorm"
)

type forkSnapshotArg []store.ContextMessage

func (want forkSnapshotArg) Match(value driver.Value) bool {
	var raw []byte
	switch value := value.(type) {
	case []byte:
		raw = value
	case string:
		raw = []byte(value)
	default:
		return false
	}
	var got []store.ContextMessage
	return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, []store.ContextMessage(want))
}

func expectForkParent(mock sqlmock.Sqlmock, inherited []store.ContextMessage) {
	var parentID any
	var snapshot any
	if inherited != nil {
		parentID = 1
		encoded, _ := json.Marshal(inherited)
		snapshot = encoded
	}
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE .*id.*ORDER BY .*LIMIT`).WithArgs(7, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "brief", "brief_until_seq", "parent_conversation_id", "fork_snapshot"}).
			AddRow(7, "主线", "不得复制的分叉后摘要", 100, parentID, snapshot))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE conversation_id = .*source_tag = .*ORDER BY .*LIMIT`).
		WithArgs(7, store.SourceChat, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(70, 7, "chat"))
}

func expectForkMessages(mock sqlmock.Sqlmock, cutoff int, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*seq <= .*ORDER BY seq LIMIT`).
		WithArgs(70, cutoff, MaxForkMessages+1).WillReturnRows(rows)
}

func expectForkInserts(mock sqlmock.Sqlmock, title string, snapshot []store.ContextMessage, sessionErr error) {
	mock.ExpectQuery(`INSERT INTO "conversations"`).
		WithArgs(title, "", 0, 7, 70, 2, forkSnapshotArg(snapshot), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(8))
	query := mock.ExpectQuery(`INSERT INTO "sessions"`).
		WithArgs(8, store.SourceChat, "chat", "", store.ExtractSkipped, "", nil, sqlmock.AnyArg(), sqlmock.AnyArg())
	if sessionErr != nil {
		query.WillReturnError(sessionErr)
	} else {
		query.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(80))
	}
}

func TestForkConversationFreezesSourcePrefix(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "root"
		if nested {
			name = "branch_of_branch"
		}
		t.Run(name, func(t *testing.T) {
			manager, mock := lifecycleTestManager(t)
			var inherited []store.ContextMessage
			if nested {
				inherited = []store.ContextMessage{{SessionID: 10, Seq: 5, Role: "user", Content: "祖先原文"}}
			}
			want := append(append([]store.ContextMessage{}, inherited...),
				store.ContextMessage{SessionID: 70, Seq: 1, Role: "user", Content: "讨论 Go"},
				store.ContextMessage{SessionID: 70, Seq: 2, Role: "assistant", Content: "相关建议"})
			mock.ExpectBegin()
			expectForkParent(mock, inherited)
			expectForkMessages(mock, 2, sqlmock.NewRows([]string{"seq", "role", "content"}).
				AddRow(1, "user", "讨论 Go").AddRow(2, "assistant", "相关建议"))
			expectForkInserts(mock, "选型分支", want, nil)
			mock.ExpectCommit()
			got, err := manager.ForkConversation(context.Background(), 7, 2, "  选型分支  ")
			if err != nil {
				t.Fatal(err)
			}
			if got.Conversation.ID != 8 || got.SessionID != 80 || got.InheritedCount != len(want) || !got.Conversation.IsBranch() ||
				got.Conversation.ParentConversationID == nil || *got.Conversation.ParentConversationID != 7 ||
				got.Conversation.ForkSessionID == nil || *got.Conversation.ForkSessionID != 70 || got.Conversation.ForkSeq != 2 {
				t.Fatalf("invalid branch metadata: %+v", got)
			}
			if got.Conversation.Brief != "" || got.Conversation.BriefUntilSeq != 0 {
				t.Fatalf("parent's future summary leaked: %+v", got.Conversation)
			}
			snapshot, err := got.Conversation.InheritedMessages()
			if err != nil || !reflect.DeepEqual(snapshot, want) {
				t.Fatalf("source provenance/order/roles lost: %+v, %v", snapshot, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForkConversationRejectsInvalidArgumentsBeforeSQL(t *testing.T) {
	manager := NewMemoryManger(nil, nil)
	for _, tc := range []struct {
		parent uint
		seq    int
		title  string
	}{{0, 1, ""}, {7, 0, ""}, {7, -1, ""}, {7, 1, strings.Repeat("字", 192)}} {
		result, err := manager.ForkConversation(context.Background(), tc.parent, tc.seq, tc.title)
		if !errors.Is(err, ErrInvalidFork) || result.Conversation.ID != 0 || result.SessionID != 0 {
			t.Fatalf("invalid request reached storage or returned success: %+v, %v", result, err)
		}
	}
}

func TestForkConversationRollsBackInvalidPrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows func() *sqlmock.Rows
		want error
	}{
		{"missing_cutoff", func() *sqlmock.Rows {
			return sqlmock.NewRows([]string{"seq", "role", "content"}).AddRow(1, "user", "earlier")
		}, ErrInvalidFork},
		{"empty_prefix", func() *sqlmock.Rows { return sqlmock.NewRows([]string{"seq", "role", "content"}) }, ErrInvalidFork},
		{"unsupported_role", func() *sqlmock.Rows {
			return sqlmock.NewRows([]string{"seq", "role", "content"}).AddRow(2, "system", "not inherited")
		}, ErrInvalidFork},
		{"oversize_snapshot", func() *sqlmock.Rows {
			return sqlmock.NewRows([]string{"seq", "role", "content"}).AddRow(2, "user", strings.Repeat("x", MaxForkSnapshotBytes))
		}, ErrForkTooLarge},
		{"too_many_messages", func() *sqlmock.Rows {
			rows := sqlmock.NewRows([]string{"seq", "role", "content"})
			for i := 1; i <= MaxForkMessages+1; i++ {
				rows.AddRow(i, "user", "text")
			}
			return rows
		}, ErrForkTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, mock := lifecycleTestManager(t)
			cutoff := 2
			if tc.name == "too_many_messages" {
				cutoff = MaxForkMessages + 1
			}
			mock.ExpectBegin()
			expectForkParent(mock, nil)
			expectForkMessages(mock, cutoff, tc.rows())
			mock.ExpectRollback()
			result, err := manager.ForkConversation(context.Background(), 7, cutoff, "")
			if !errors.Is(err, tc.want) || result.Conversation.ID != 0 || result.SessionID != 0 {
				t.Fatalf("invalid prefix did not fail closed: %+v, %v", result, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForkConversationSessionFailureReturnsNoPartialBranch(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	want := []store.ContextMessage{{SessionID: 70, Seq: 2, Role: "user", Content: "source"}}
	mock.ExpectBegin()
	expectForkParent(mock, nil)
	expectForkMessages(mock, 2, sqlmock.NewRows([]string{"seq", "role", "content"}).AddRow(2, "user", "source"))
	failure := errors.New("session insert failed")
	expectForkInserts(mock, "分支对话", want, failure)
	mock.ExpectRollback()
	result, err := manager.ForkConversation(context.Background(), 7, 2, " ")
	if !errors.Is(err, failure) || result.Conversation.ID != 0 || result.SessionID != 0 {
		t.Fatalf("partial branch returned: %+v, %v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestForkConversationMissingParent(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "conversations"`).WithArgs(7, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()
	result, err := manager.ForkConversation(context.Background(), 7, 2, "")
	if !errors.Is(err, gorm.ErrRecordNotFound) || result.Conversation.ID != 0 {
		t.Fatalf("missing parent did not propagate: %+v, %v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
