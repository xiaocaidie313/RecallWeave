package http

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recallweave/internal/ingest"
	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestImportConversationRejectsBadID(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/abc/imports", strings.NewReader(`{"source_tag":"wx","text":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

const markdownImportFixture = "# 项目讨论\n\n## User\n数据库选择 PostgreSQL。\n\n## Assistant\n使用 pgvector 检索记忆。\n"

func markdownImportHandler(t *testing.T) (*IngestHandler, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	database, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return NewIngestHandler(ingest.NewService(database, nil)), mock
}

// The real handler, service and parser must preserve source identity and roles
// while committing both messages with their session. No extractor is injected.
func expectMarkdownImport(mock sqlmock.Sqlmock, bound bool) {
	var owner any
	if bound {
		owner = uint(7)
		mock.ExpectQuery(`SELECT count\(\*\) FROM "conversations" WHERE id =`).WithArgs(uint(7)).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "sessions" .*RETURNING "id"`).
		WithArgs(owner, types.SourceChatGPT, "项目讨论", markdownImportFixture, types.ExtractPending, "", nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12))
	mock.ExpectQuery(`INSERT INTO "messages" .*RETURNING "id"`).
		WithArgs(types.SourceChatGPT, uint(12), 1, "user", "数据库选择 PostgreSQL。", sqlmock.AnyArg(), sqlmock.AnyArg(),
			types.SourceChatGPT, uint(12), 2, "assistant", "使用 pgvector 检索记忆。", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(31).AddRow(32))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "sessions" SET "extract_status"=.*"updated_at"=.*WHERE id =`).
		WithArgs(types.ExtractSkipped, sqlmock.AnyArg(), uint(12)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func assertMarkdownImported(t *testing.T, handler *IngestHandler, mock sqlmock.Sqlmock, request *http.Request) {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewRouter(handler, nil, nil, nil, nil, nil).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected Markdown import success, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"sessions":[{"extract_status":"skipped","message_count":2,"session_id":12}]}` {
		t.Fatalf("unexpected import response: %s", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestImportMarkdownTextHonorsExplicitFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		bound bool
	}{
		{"global", "/api/imports", false},
		{"bound", "/api/conversations/7/imports", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, mock := markdownImportHandler(t)
			expectMarkdownImport(mock, tc.bound)
			body, err := json.Marshal(map[string]string{"source_tag": "chatgpt", "text": markdownImportFixture, "format": "markdown"})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			assertMarkdownImported(t, handler, mock, request)
		})
	}
}

func markdownFileRequest(t *testing.T, path, filename, format string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"source_tag": "chatgpt", "format": format} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestImportMarkdownFileDetectsAndOverridesFormat(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("export/conversation.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(markdownImportFixture)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, filename, format string
		content                []byte
		bound                  bool
	}{
		{"md extension", "conversation.md", "", []byte(markdownImportFixture), false},
		{"markdown extension bound", "conversation.markdown", "", []byte(markdownImportFixture), true},
		{"zip inner markdown", "export.zip", "", archive.Bytes(), false},
		{"explicit format overrides txt", "conversation.txt", "markdown", []byte(markdownImportFixture), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, mock := markdownImportHandler(t)
			expectMarkdownImport(mock, tc.bound)
			path := "/api/imports/file"
			if tc.bound {
				path = "/api/conversations/7/imports/file"
			}
			assertMarkdownImported(t, handler, mock, markdownFileRequest(t, path, tc.filename, tc.format, tc.content))
		})
	}
}

func TestImportRejectsInvalidFormat(t *testing.T) {
	for _, path := range []string{"/api/imports", "/api/conversations/7/imports", "/api/imports/file", "/api/conversations/7/imports/file"} {
		t.Run(path, func(t *testing.T) {
			// A nil database makes accidental parsing/storage access fail the test.
			handler := NewIngestHandler(ingest.NewService(nil, nil))
			var request *http.Request
			if strings.HasSuffix(path, "/file") {
				request = markdownFileRequest(t, path, "conversation.md", "html", []byte(markdownImportFixture))
			} else {
				body, err := json.Marshal(map[string]string{"source_tag": "chatgpt", "text": markdownImportFixture, "format": "html"})
				if err != nil {
					t.Fatal(err)
				}
				request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
			}
			recorder := httptest.NewRecorder()
			NewRouter(handler, nil, nil, nil, nil, nil).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), ingest.ErrInvalidFormat.Error()) {
				t.Fatalf("expected invalid format rejection, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestImportConversationRejectsMissingText(t *testing.T) {
	router := NewRouter(NewIngestHandler(nil), nil, nil, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/1/imports", strings.NewReader(`{"source_tag":"wx"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}
