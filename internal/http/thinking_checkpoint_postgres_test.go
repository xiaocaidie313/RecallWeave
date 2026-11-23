//go:build integration

package http_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/conversation"
	apphttp "recallweave/internal/http"
	"recallweave/internal/thinking"
	"recallweave/internal/types"

	"gorm.io/gorm"
)

func TestPostgresHTTPThinkingCheckpoint(t *testing.T) {
	ctx, database, dsn := workflowPostgres(t)
	startServer := func(database *gorm.DB) *httptest.Server {
		conversations := apphttp.NewConversationHandler(conversation.NewConversationManager(database), nil)
		router := apphttp.NewRouter(nil, nil, nil, conversations, nil, nil)
		apphttp.MountThinkingCheckpoints(router, thinking.NewStore(database))
		server := httptest.NewServer(router)
		t.Cleanup(server.Close)
		return server
	}
	server := startServer(database)
	createPath := func(title string) string {
		var item types.Conversation
		workflowJSON(t, ctx, server, http.MethodPost, "/api/conversations/create", map[string]string{"title": title}, http.StatusCreated, &item)
		return fmt.Sprintf("/api/conversations/%d/thinking/checkpoint", item.ID)
	}
	firstPath := createPath("散步和写信")
	otherPath := createPath("另一个问题")
	const absentPath = "/api/conversations/99999/thinking/checkpoint"

	workflowJSON(t, ctx, server, http.MethodGet, absentPath, nil, http.StatusNotFound, nil)
	workflowJSON(t, ctx, server, http.MethodPost, absentPath, map[string]string{"text": "新问题"}, http.StatusNotFound, nil)
	var response struct{ Checkpoint *types.ThinkingCheckpoint }
	workflowJSON(t, ctx, server, http.MethodGet, firstPath, nil, http.StatusOK, &response)
	if response.Checkpoint != nil {
		t.Fatalf("new conversation has a checkpoint: %+v", response.Checkpoint)
	}
	workflowJSON(t, ctx, server, http.MethodPost, firstPath, map[string]string{"text": "追问回应的作用", "direction": "explore"}, http.StatusOK, &response)
	const revised = "真正有用的是把事情讲给一个具体的人吗？"
	newCheckpoint := map[string]string{"text": revised, "direction": "reframe", "author": "assistant"}
	workflowJSON(t, ctx, server, http.MethodPost, firstPath, newCheckpoint, http.StatusOK, &response)
	if response.Checkpoint == nil || response.Checkpoint.Text != revised ||
		response.Checkpoint.Author != "user" || response.Checkpoint.Direction != "reframe" {
		t.Fatalf("last explicit user checkpoint not preserved: %+v", response.Checkpoint)
	}
	written := *response.Checkpoint
	server.Close()
	server = startServer(workflowOpenDatabase(t, dsn))
	workflowJSON(t, ctx, server, http.MethodGet, firstPath, nil, http.StatusOK, &response)
	if response.Checkpoint == nil || *response.Checkpoint != written {
		t.Fatalf("reopening the service changed the checkpoint: before %+v, after %+v", written, response.Checkpoint)
	}
	workflowJSON(t, ctx, server, http.MethodGet, otherPath, nil, http.StatusOK, &response)
	if response.Checkpoint != nil {
		t.Fatalf("another conversation received the saved checkpoint: %+v", response.Checkpoint)
	}
}
