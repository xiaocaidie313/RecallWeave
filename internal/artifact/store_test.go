package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSavePreservesMarkdownAndConfinesFilename(t *testing.T) {
	s := openTestStore(t)
	markdown := "# 偏好记录\n\n我喜欢马思唯。来源：会话 1，消息 23。\n"
	saved, err := s.Save(context.Background(), "../../outside:C:\\test", markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.ID) != 32 || saved.Filename != saved.ID+".md" || filepath.Dir(saved.Path) != s.dir || saved.Bytes != len(markdown) {
		t.Fatalf("unexpected output location: %+v", saved)
	}
	data, err := os.ReadFile(saved.Path)
	if err != nil || string(data) != markdown {
		t.Fatalf("Markdown changed: %q, %v", data, err)
	}
	second, err := s.Save(context.Background(), "相同标题", markdown)
	if err != nil || second.ID == saved.ID {
		t.Fatalf("repeated save must create a distinct file: %+v, %v", second, err)
	}
}

func TestSaveNeverOverwritesExistingFile(t *testing.T) {
	s := openTestStore(t)
	// Force an ID collision instead of relying on random probability.
	s.random = bytes.NewReader(make([]byte, 32))
	first, err := s.Save(context.Background(), "first", "original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(context.Background(), "second", "replacement"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected exclusive-create collision, got %v", err)
	}
	data, err := os.ReadFile(first.Path)
	if err != nil || string(data) != "original" {
		t.Fatalf("existing file modified: %q, %v", data, err)
	}
}

func TestSaveRejectsInvalidInputWithoutFiles(t *testing.T) {
	s := openTestStore(t)
	for _, tc := range []struct{ title, content string }{
		{"", "text"}, {"   ", "text"}, {strings.Repeat("字", 201), "text"},
		{"title", " \n"}, {"title", strings.Repeat("x", MaxMarkdownBytes+1)},
		{"title", string([]byte{0xff})},
	} {
		if _, err := s.Save(context.Background(), tc.title, tc.content); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Save(ctx, "title", "text"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid input left output files: %v, %v", entries, err)
	}
}

func TestConcurrentSaves(t *testing.T) {
	s := openTestStore(t)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := s.Save(context.Background(), "title", "content"); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	entries, err := os.ReadDir(s.dir)
	if err != nil || len(entries) != 8 {
		t.Fatalf("expected eight distinct files: %d, %v", len(entries), err)
	}
}

func TestSaveDoesNotFollowExistingSymlink(t *testing.T) {
	s := openTestStore(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.dir, strings.Repeat("0", 32)+".md")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	s.random = bytes.NewReader(make([]byte, 16))
	if _, err := s.Save(context.Background(), "title", "replacement"); err == nil {
		t.Fatal("existing symlink accepted")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "keep" {
		t.Fatalf("outside file modified: %q, %v", data, err)
	}
}
