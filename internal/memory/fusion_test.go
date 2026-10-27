package memory

import (
	"context"
	"errors"
	"slices"
	"testing"

	"recallweave/internal/retrieval"
	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
)

func fusionMemories(ids ...uint) []store.Memory {
	items := make([]store.Memory, len(ids))
	for i, id := range ids {
		items[i] = store.Memory{ID: id}
	}
	return items
}

func fusionIDs(items []store.Memory) []uint {
	ids := make([]uint, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func TestMergeMemoriesRRF(t *testing.T) {
	for _, tc := range []struct {
		name            string
		vector, keyword []uint
		limit           int
		want            []uint
	}{
		{"keyword can enter a full vector list", []uint{1, 2, 3, 4, 5}, []uint{99}, 5, []uint{99, 1, 2, 3, 4}},
		{"joint hit below final cutoff is considered", []uint{1, 2, 3, 42}, []uint{50, 49, 48, 42}, 2, []uint{42, 50}},
		{"both routes boost one item without duplicating it", []uint{1, 2, 3}, []uint{4, 5, 3}, 3, []uint{3, 4, 1}},
		{"duplicates in one route do not add votes", []uint{1, 1, 1, 2}, []uint{3}, 3, []uint{3, 1, 2}},
		{"vector only preserves rank", []uint{4, 2, 9}, nil, 2, []uint{4, 2}},
		{"keyword only preserves rank", nil, []uint{2, 7, 1}, 3, []uint{2, 7, 1}},
		{"no hits", nil, nil, 5, nil},
		{"zero limit", []uint{1}, []uint{2}, 0, nil},
		{"negative limit", []uint{1}, []uint{2}, -1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vector, keyword := fusionMemories(tc.vector...), fusionMemories(tc.keyword...)
			for range 20 {
				got := mergeMemories(vector, keyword, tc.limit)
				if !slices.Equal(fusionIDs(got), tc.want) {
					t.Fatalf("got %v, want %v", fusionIDs(got), tc.want)
				}
				// Equal weights must not give the first route preferential ranking.
				swapped := mergeMemories(keyword, vector, tc.limit)
				if !slices.Equal(fusionIDs(swapped), tc.want) {
					t.Fatalf("route order changed ranking: %v", fusionIDs(swapped))
				}
			}
			if !slices.Equal(fusionIDs(vector), tc.vector) || !slices.Equal(fusionIDs(keyword), tc.keyword) {
				t.Fatal("fusion mutated an input ranking")
			}
		})
	}
}

func TestFusionPreservesSourceLocation(t *testing.T) {
	item := store.Memory{ID: 7, SessionID: 3, SeqStart: 23, SeqEnd: 24, Title: "偏好", Content: "我喜欢马思唯", Status: store.MemoryActive}
	got := mergeMemories([]store.Memory{item}, []store.Memory{item}, 5)
	if len(got) != 1 || got[0].SessionID != 3 || got[0].SeqStart != 23 || got[0].SeqEnd != 24 || got[0].Content != item.Content || got[0].Status != item.Status {
		t.Fatalf("source location or content changed: %+v", got)
	}
}

type fusionEmbedder struct {
	calls int
	err   error
}

func (e *fusionEmbedder) Embed(context.Context, string) ([]float64, error) {
	e.calls++
	values := make([]float64, store.EmbeddingDim)
	values[0] = 1
	return values, e.err
}

func TestFindMemoriesFusesCandidateWindowsWithFilters(t *testing.T) {
	for _, history := range []bool{false, true} {
		name := "active_only"
		if history {
			name = "include_inactive"
		}
		t.Run(name, func(t *testing.T) {
			manager, mock := lifecycleTestManager(t)
			embedder := &fusionEmbedder{}
			manager.retriever = retrieval.New(manager.db, embedder)
			tag := store.MemoryTag("people")
			keywordRows := sqlmock.NewRows([]string{"id"}).AddRow(50).AddRow(49).AddRow(48).AddRow(42)
			if history {
				mock.ExpectQuery(`SELECT .* FROM "memories" WHERE .*tag = \$3 ORDER BY id DESC LIMIT \$4`).
					WithArgs("%说唱%", "%说唱%", tag, 10).WillReturnRows(keywordRows)
			} else {
				mock.ExpectQuery(`SELECT .* FROM "memories" WHERE status = \$1.*tag = \$4 ORDER BY id DESC LIMIT \$5`).
					WithArgs(store.MemoryActive, "%说唱%", "%说唱%", tag, 10).WillReturnRows(keywordRows)
			}
			vectorRows := sqlmock.NewRows([]string{"id", "embedding"})
			for i, id := range []uint{1, 2, 3, 42} {
				values := make([]float64, store.EmbeddingDim)
				values[0], values[1] = 1, float64(i)/2
				vectorRows.AddRow(id, store.NewEmbedding(values))
			}
			if history {
				mock.ExpectQuery(`SELECT .* FROM "memories" WHERE embedding IS NOT NULL AND tag = \$1 ORDER BY embedding <=> \$2 LIMIT \$3`).
					WithArgs(tag, sqlmock.AnyArg(), 10).WillReturnRows(vectorRows)
			} else {
				mock.ExpectQuery(`SELECT .* FROM "memories" WHERE embedding IS NOT NULL AND status = \$1 AND tag = \$2 ORDER BY embedding <=> \$3 LIMIT \$4`).
					WithArgs(store.MemoryActive, tag, sqlmock.AnyArg(), 10).WillReturnRows(vectorRows)
			}
			got, err := manager.FindMemories(context.Background(), "说唱", tag, 2, history)
			if err != nil || !slices.Equal(fusionIDs(got), []uint{42, 50}) {
				t.Fatalf("candidate windows were truncated before fusion: %v, %v", fusionIDs(got), err)
			}
			if embedder.calls != 1 {
				t.Fatalf("expected one embedding call, got %d", embedder.calls)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFindMemoriesVectorFailureFallsBackToKeywords(t *testing.T) {
	manager, mock := lifecycleTestManager(t)
	embedder := &fusionEmbedder{err: errors.New("embedding unavailable")}
	manager.retriever = retrieval.New(manager.db, embedder)
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE status = \$1.*LIMIT \$4`).
		WithArgs(store.MemoryActive, "%偏好%", "%偏好%", 10).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9).AddRow(8).AddRow(7))
	got, err := manager.FindMemories(context.Background(), "偏好", "", 2)
	if err != nil || !slices.Equal(fusionIDs(got), []uint{9, 8}) || embedder.calls != 1 {
		t.Fatalf("keyword fallback failed: %v, %v", fusionIDs(got), err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFindMemoriesBoundsCandidateLimit(t *testing.T) {
	for _, tc := range []struct{ limit, candidates int }{{0, 25}, {-1, 25}, {1000, 100}} {
		manager, mock := lifecycleTestManager(t)
		mock.ExpectQuery(`SELECT .* FROM "memories" WHERE status = \$1.*LIMIT \$4`).
			WithArgs(store.MemoryActive, "%偏好%", "%偏好%", tc.candidates).
			WillReturnRows(sqlmock.NewRows([]string{"id"}))
		if _, err := manager.FindMemories(context.Background(), "偏好", "", tc.limit); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}
