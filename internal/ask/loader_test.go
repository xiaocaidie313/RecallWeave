package ask

import (
	"strings"
	"testing"

	"recallweave/internal/store"
)

func TestFormatImportedBackground(t *testing.T) {
	got := formatImportedBackground([]importedPiece{{
		session: store.Session{ID: 12, Name: "和张三", SourceTag: store.SourceWX},
		memories: []store.Memory{{
			ID: 4, SessionID: 12, SeqStart: 19, SeqEnd: 30, Tag: store.TagDecision, Title: "后端用 gin",
		}},
	}})
	if strings.Contains(got, "近期原文") {
		t.Fatalf("bound import should stay a catalog, got %q", got)
	}
	if !strings.Contains(got, "会话 12「和张三」（wx）") || !strings.Contains(got, "memory 4，消息 19-30") {
		t.Fatalf("catalog missing memory location: %q", got)
	}

	if formatImportedBackground(nil) != "" {
		t.Fatal("empty import should not add background")
	}

	unnamed := formatImportedBackground([]importedPiece{{
		session:  store.Session{ID: 3, SourceTag: store.SourceWX},
		memories: []store.Memory{{ID: 1, Title: "一条", Tag: store.TagLife, SeqStart: 1, SeqEnd: 2}},
	}})
	if !strings.Contains(unnamed, "会话 3「未命名」（wx）") || strings.Contains(unnamed, "「wx」") {
		t.Fatalf("empty name should not reuse the source tag as a title, got %q", unnamed)
	}
}

func TestClipMemories(t *testing.T) {
	memories := make([]store.Memory, 0, 10)
	for i := 1; i <= 10; i++ {
		memories = append(memories, store.Memory{ID: uint(i), Title: "一条"})
	}

	taken, left, truncated := truncateMemories(memories, maxCatalogTotal)
	if len(taken) != maxCatalogPerSession || left != maxCatalogTotal-maxCatalogPerSession || !truncated {
		t.Fatalf("per-session cap: taken=%d left=%d truncated=%v", len(taken), left, truncated)
	}

	taken, left, truncated = truncateMemories(memories[:3], 2)
	if len(taken) != 2 || left != 0 || !truncated {
		t.Fatalf("total cap: taken=%d left=%d truncated=%v", len(taken), left, truncated)
	}
}
