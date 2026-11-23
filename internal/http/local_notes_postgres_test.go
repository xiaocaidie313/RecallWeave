//go:build integration

package http_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"recallweave/internal/conversation"
	apphttp "recallweave/internal/http"
	"recallweave/internal/ingest"
	"recallweave/internal/session"
	"recallweave/internal/types"
)

func TestPostgresLocalNoteFiles(t *testing.T) {
	ctx, db, _ := workflowPostgres(t)
	sessions := session.NewSessionManager(db)
	conversations := conversation.NewConversationManager(db)
	router := apphttp.NewRouter(apphttp.NewIngestHandler(ingest.NewService(db, nil)), nil, nil,
		apphttp.NewConversationHandler(conversations, sessions), nil, nil)
	server := httptest.NewServer(router)
	defer server.Close()
	parent, err := conversations.CreateConversation(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	branch := types.Conversation{Title: "branch", ParentConversationID: &parent.ID,
		ForkSnapshot: json.RawMessage(`[{"session_id":1000,"seq":1,"role":"user","content":"早先的问题"}]`)}
	if err := db.Create(&branch).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/conversations/%d", branch.ID)
	var wantNotes []session.LocalNote
	for _, tc := range []struct {
		name, content string
		parts         []string
	}{
		{"我的笔记.md", "\ufeff# 一个想法\r\n\r\n写下问题。\r\n\r\n## 另一面\r\n\r\n换个角度。", []string{"# 一个想法\n\n写下问题。", "## 另一面\n\n换个角度。"}},
		{"我的笔记.txt", "\ufeff第一段。\r\n\r\n第二段。", []string{"第一段。", "第二段。"}},
	} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("source_tag", "local_note"); err != nil {
			t.Fatal(err)
		}
		if err := form.WriteField("name", tc.name); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(tc.content)); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path+"/imports/file", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		var result struct {
			Sessions []struct {
				ID uint `json:"session_id"`
			}
		}
		if res.Code != http.StatusCreated || json.Unmarshal(res.Body.Bytes(), &result) != nil || len(result.Sessions) != 1 {
			t.Fatalf("%s upload: %d %s", tc.name, res.Code, res.Body.String())
		}
		id := result.Sessions[0].ID
		stored, err := sessions.GetSession(ctx, id)
		if err != nil || stored.RawText != tc.content || stored.SourceTag != types.SourceLocalNote {
			t.Fatalf("%s original changed: %+v, %v", tc.name, stored, err)
		}
		parts, err := sessions.LocalNoteMessages(ctx, branch.ID, id, 1, 2)
		if err != nil || len(parts) != len(tc.parts) {
			t.Fatalf("%s own selection: %+v, %v", tc.name, parts, err)
		}
		for i, part := range parts {
			if part.Seq != i+1 || part.Content != tc.parts[i] || part.Role != "user" {
				t.Fatalf("%s passage %d: %+v", tc.name, i+1, part)
			}
		}
		if other, err := sessions.LocalNoteMessages(ctx, parent.ID, id, 1, 2); err != nil || len(other) != 0 {
			t.Fatalf("foreign conversation selected %s: %+v, %v", tc.name, other, err)
		}
		wantNotes = append([]session.LocalNote{{SessionID: id, Name: tc.name, ExtractStatus: types.ExtractSkipped}}, wantNotes...)
	}
	if _, err := sessions.ChatSession(ctx, branch.ID); err != nil {
		t.Fatal(err)
	}
	var list struct{ Notes []session.LocalNote }
	workflowJSON(t, ctx, server, http.MethodGet, path+"/notes", nil, http.StatusOK, &list)
	if !reflect.DeepEqual(list.Notes, wantNotes) {
		t.Fatalf("wrong note list: %+v", list.Notes)
	}
	raw := workflowJSON(t, ctx, server, http.MethodGet, fmt.Sprintf("/api/conversations/%d/notes", parent.ID), nil, http.StatusOK, nil)
	if string(raw) != `{"notes":[]}` {
		t.Fatalf("empty parent list: %s", raw)
	}
	workflowJSON(t, ctx, server, http.MethodGet, "/api/conversations/9999999/notes", nil, http.StatusNotFound, nil)

	thinking := thinkingWorkflowServer(t, db)
	turn := map[string]any{"mode": "thinking", "question": "读回选中的第二段", "source": types.ThoughtSource{
		SessionID: wantNotes[0].SessionID, SeqStart: 2, SeqEnd: 2,
	}}
	workflowJSON(t, ctx, thinking, http.MethodPost, fmt.Sprintf("/api/chat/%d", branch.ID), turn, http.StatusOK, nil)
	sibling := types.Conversation{Title: "sibling", ParentConversationID: &parent.ID, ForkSnapshot: branch.ForkSnapshot}
	if err := db.Create(&sibling).Error; err != nil {
		t.Fatal(err)
	}
	workflowJSON(t, ctx, thinking, http.MethodPost, fmt.Sprintf("/api/chat/%d", sibling.ID), turn, http.StatusNotFound, nil)

	// A single INSERT for this many messages exceeds the extended-query parameter limit.
	const paragraphs = 8200
	large, err := ingest.NewService(db, nil).ImportText(ctx, types.SourceLocalNote, "long.txt", strings.Repeat("a\n\n", paragraphs))
	if err != nil || len(large) != 1 || large[0].MessageCount != paragraphs {
		t.Fatalf("large note import: %+v, %v", large, err)
	}
	rows, err := sessions.GetMessages(ctx, large[0].SessionID, paragraphs, paragraphs)
	if err != nil || len(rows) != 1 || rows[0].Seq != paragraphs {
		t.Fatalf("last paragraph: %+v, %v", rows, err)
	}
}
