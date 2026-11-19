//go:build integration

package http_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/extract"
	apphttp "recallweave/internal/http"
	"recallweave/internal/memory"
	"recallweave/internal/types"
)

func TestPostgresHTTPMemorySearchWithoutModel(t *testing.T) {
	ctx, database, _ := workflowPostgres(t)
	session := types.Session{SourceTag: types.SourceCodex, Name: "search acceptance"}
	if err := database.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	message := types.Message{SessionID: session.ID, Seq: 1, SourceTag: types.SourceCodex, Role: "user", Content: "项目选择 PostgreSQL 保存记忆和原文。"}
	if err := database.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := extract.NewExtractExcuter(database, nil, nil).ExtractSessionToMemory(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	router := apphttp.NewRouter(nil, nil, nil, nil, nil, nil)
	apphttp.MountMemorySearch(router, memory.NewMemoryManger(database, nil))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	search := func(suffix string) []types.Memory {
		t.Helper()
		response, err := server.Client().Get(server.URL + "/api/memories/search?q=PostgreSQL" + suffix)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result struct {
			Memories []types.Memory `json:"memories"`
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("search failed: %d", response.StatusCode)
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Memories
	}
	hits := search("")
	if len(hits) != 1 || hits[0].SessionID != session.ID || hits[0].SeqStart != 1 || hits[0].SeqEnd != 1 {
		t.Fatalf("search did not preserve original-message location: %+v", hits)
	}
	var stored types.Memory
	if err := database.First(&stored, hits[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RecalledCount != 0 {
		t.Fatal("read-only search increased recall activity")
	}
	if err := database.Model(&stored).Update("status", types.MemoryInactive).Error; err != nil {
		t.Fatal(err)
	}
	if len(search("")) != 0 || len(search("&include_inactive=true")) != 1 {
		t.Fatal("inactive memory filtering is incorrect")
	}
}
