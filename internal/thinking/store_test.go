package thinking

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func testStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = conn.Close()
	})
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db), mock
}

func TestCheckpointSave(t *testing.T) {
	store, mock := testStore(t)
	text := "  什么条件下我会改变想法？\n"
	columns := []string{"conversation_id", "text", "direction", "author", "updated_at"}
	const insert = `INSERT INTO thinking_checkpoints AS prior .*SELECT id, \$1, \$2, 'user', statement_timestamp\(\) FROM conversations WHERE id = \$3 FOR KEY SHARE ON CONFLICT \(conversation_id\) DO UPDATE`
	now := time.Now().UTC()
	mock.ExpectQuery(insert).WithArgs(text, "reframe", 17).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(17, text, "reframe", "user", now))
	note, err := store.Save(context.Background(), 17, text, "reframe")
	if err != nil || note == nil || note.Text != text || note.Author != "user" || note.Direction != "reframe" || note.ConversationID != 17 || !note.UpdatedAt.Equal(now) {
		t.Fatalf("saved checkpoint = %+v, error = %v", note, err)
	}
	mock.ExpectQuery(insert).WithArgs(text, "", 18).WillReturnRows(sqlmock.NewRows(columns))
	note, err = store.Save(context.Background(), 18, text, "")
	if note != nil || !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("save to unknown conversation = %+v, error = %v", note, err)
	}
}

func TestCheckpointLatest(t *testing.T) {
	store, mock := testStore(t)
	columns := []string{"conversation_id", "text", "direction", "author", "updated_at"}
	const read = `SELECT tc\.\* .* LEFT JOIN thinking_checkpoints AS tc ON tc.conversation_id = c.id WHERE c.id = \$1`
	now := time.Now().UTC()
	mock.ExpectQuery(read).WithArgs(7).WillReturnRows(sqlmock.NewRows(columns).AddRow(7, "我的问题", "", "user", now))
	note, err := store.Latest(context.Background(), 7)
	if err != nil || note == nil || note.ConversationID != 7 || note.Text != "我的问题" || note.Author != "user" || !note.UpdatedAt.Equal(now) {
		t.Fatalf("latest checkpoint = %+v, error = %v", note, err)
	}
	mock.ExpectQuery(read).WithArgs(8).WillReturnRows(sqlmock.NewRows(columns).AddRow(nil, nil, nil, nil, nil))
	note, err = store.Latest(context.Background(), 8)
	if note != nil || err != nil {
		t.Fatalf("existing conversation without checkpoint = %+v, error = %v", note, err)
	}
	mock.ExpectQuery(read).WithArgs(9).WillReturnRows(sqlmock.NewRows(columns))
	note, err = store.Latest(context.Background(), 9)
	if note != nil || !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("unknown conversation = %+v, error = %v", note, err)
	}
}

func TestCheckpointInvalid(t *testing.T) {
	store, _ := testStore(t)
	for _, test := range []struct{ text, direction string }{
		{"\n 　 ", ""}, {strings.Repeat("字", MaxCheckpointTextRunes+1), ""}, {"\xff", ""}, {"零\x00字符", ""}, {"明天再想", "assistant"},
	} {
		note, err := store.Save(context.Background(), 1, test.text, test.direction)
		if note != nil || !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("invalid checkpoint should fail before any SQL, got %+v, %v", note, err)
		}
	}
}
