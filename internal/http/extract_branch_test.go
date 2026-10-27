package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/extract"
	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestManualBranchExtractionReturnsConflict(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .*conversations.parent_conversation_id FROM "sessions" LEFT JOIN conversations ON conversations.id = sessions.conversation_id WHERE sessions.id = \$1 LIMIT \$2`).
		WithArgs(10, 1).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "conversation_id", "found_conversation_id", "parent_conversation_id"}).AddRow(10, 3, 3, 1))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "sessions" SET `).
		WithArgs("", store.ExtractSkipped, sqlmock.AnyArg(), 10).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	router := NewRouter(nil, NewExtractHandler(extract.NewExtractExcuter(db, nil, nil)), nil, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/sessions/10/extract", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("branch extraction should be blocked, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
