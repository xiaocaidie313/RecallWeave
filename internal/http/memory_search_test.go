package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"recallweave/internal/types"
)

type searchFinder struct {
	query    string
	tag      types.MemoryTag
	limit    int
	inactive bool
	err      error
}

func (f *searchFinder) FindMemories(_ context.Context, q string, tag types.MemoryTag, limit int, inactive ...bool) ([]types.Memory, error) {
	f.query, f.tag, f.limit, f.inactive = q, tag, limit, inactive[0]
	if f.err != nil {
		return nil, f.err
	}
	if q == "empty" {
		return nil, nil
	}
	return []types.Memory{{ID: 7, SessionID: 3, SeqStart: 2, SeqEnd: 4, Title: "数据库选择", Content: "采用 PostgreSQL", Status: types.MemoryActive, ContentType: types.ContentDecision}}, nil
}

func TestMemorySearchReturnsEvidenceAndFilters(t *testing.T) {
	finder := &searchFinder{}
	router := NewRouter(nil, nil, nil, nil, nil, nil)
	MountMemorySearch(router, finder)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/memories/search?q=PostgreSQL&tag=tech&limit=2&include_inactive=true", nil))
	var result struct {
		Memories []memoryView `json:"memories"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(result.Memories) != 1 {
		t.Fatalf("unexpected search: %d %s", response.Code, response.Body)
	}
	hit := result.Memories[0]
	if hit.ID != 7 || hit.SessionID != 3 || hit.SeqStart != 2 || hit.SeqEnd != 4 || hit.ContentType != types.ContentDecision {
		t.Fatalf("search lost evidence provenance: %+v", hit)
	}
	if finder.query != "PostgreSQL" || finder.tag != types.TagTech || finder.limit != 2 || !finder.inactive {
		t.Fatalf("filters ignored: %+v", finder)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/memories/search?q=empty", nil))
	if response.Code != http.StatusOK || response.Body.String() != `{"memories":[]}` || finder.limit != 5 || finder.inactive {
		t.Fatalf("default/empty search mismatch: %d %s %+v", response.Code, response.Body, finder)
	}
}

func TestMemorySearchRejectsBadFiltersAndHandlesFailure(t *testing.T) {
	finder := &searchFinder{}
	router := NewRouter(nil, nil, nil, nil, nil, nil)
	MountMemorySearch(router, finder)
	for _, query := range []string{"", "q=%20", "q=x&limit=0", "q=x&limit=21", "q=x&limit=no", "q=x&tag=unknown", "q=x&include_inactive=yes"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/memories/search?"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %q got %d", query, response.Code)
		}
	}
	if finder.query != "" {
		t.Fatal("invalid request reached memory search")
	}
	finder.err = errors.New("private database details")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/memories/search?q=x", nil))
	if response.Code != http.StatusInternalServerError || response.Body.String() != `{"error":"failed to search memories"}` {
		t.Fatalf("search failure leaked details: %s", response.Body)
	}
}
