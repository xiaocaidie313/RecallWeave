package extract

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"recallweave/internal/llm"
	"recallweave/internal/store"
)

func TestFirstLine(t *testing.T) {
	if got := firstLine("第一行\n第二行"); got != "第一行" {
		t.Fatalf("expected first line, got %q", got)
	}

	if got := firstLine("  带空格的一行  "); got != "带空格的一行" {
		t.Fatalf("expected trimmed line, got %q", got)
	}

	long := strings.Repeat("记", maxTitleLength+10)
	if got := utf8.RuneCountInString(firstLine(long)); got != maxTitleLength {
		t.Fatalf("expected truncation to %d runes, got %d", maxTitleLength, got)
	}
}

// fakeSegmenter 用来替代真实模型，避免测试发网络请求。
type fakeSegmenter struct {
	segments []llm.Segment
	err      error
}

func (f fakeSegmenter) Segment(context.Context, []llm.Message, []string) ([]llm.Segment, error) {
	return f.segments, f.err
}

func testMessages() []store.Message {
	return []store.Message{
		{SessionID: 1, Seq: 1, Role: "user", Content: "第一条\n还有下文"},
		{SessionID: 1, Seq: 2, Role: "user", Content: "第二条"},
		{SessionID: 1, Seq: 3, Role: "user", Content: "第三条"},
	}
}

func TestSegmentUsesModelResult(t *testing.T) {
	excuter := NewExtractExcuter(nil, fakeSegmenter{
		segments: []llm.Segment{
			{SeqStart: 1, SeqEnd: 2, Title: "前两条", Summary: "摘要", Tag: "tech"},
			{SeqStart: 3, SeqEnd: 3, Title: "第三条", Summary: "摘要", Tag: "work"},
		},
	}, nil)

	got := excuter.segment(context.Background(), testMessages())

	if len(got) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(got))
	}
	if got[0].SeqStart != 1 || got[0].SeqEnd != 2 || got[0].Tag != "tech" {
		t.Fatalf("unexpected first segment: %+v", got[0])
	}
}

func TestSegmentDropsOutOfRangeSegments(t *testing.T) {
	excuter := NewExtractExcuter(nil, fakeSegmenter{
		segments: []llm.Segment{
			{SeqStart: 1, SeqEnd: 2, Title: "有效", Summary: "摘要", Tag: "tech"},
			{SeqStart: 4, SeqEnd: 9, Title: "编号越界", Summary: "摘要", Tag: "tech"},
			{SeqStart: 3, SeqEnd: 1, Title: "区间反向", Summary: "摘要", Tag: "tech"},
		},
	}, nil)

	got := excuter.segment(context.Background(), testMessages())

	if len(got) != 1 {
		t.Fatalf("expected only the valid segment, got %d: %+v", len(got), got)
	}
}

func TestSegmentFillsEmptyTitleAndSummary(t *testing.T) {
	excuter := NewExtractExcuter(nil, fakeSegmenter{
		segments: []llm.Segment{{SeqStart: 1, SeqEnd: 2, Tag: "tech"}},
	}, nil)

	got := excuter.segment(context.Background(), testMessages())

	if got[0].Title != "第一条" {
		t.Fatalf("expected fallback title, got %q", got[0].Title)
	}
	if got[0].Summary == "" {
		t.Fatal("expected fallback summary")
	}
}

func TestSegmentFallsBackOnError(t *testing.T) {
	excuter := NewExtractExcuter(nil, fakeSegmenter{err: errors.New("boom")}, nil)

	got := excuter.segment(context.Background(), testMessages())

	if len(got) != 1 {
		t.Fatalf("expected whole-session fallback, got %d segments", len(got))
	}
	if got[0].SeqStart != 1 || got[0].SeqEnd != 3 {
		t.Fatalf("expected fallback to cover the session, got %+v", got[0])
	}
}

func TestSegmentFallsBackWithoutSegmenter(t *testing.T) {
	excuter := NewExtractExcuter(nil, nil, nil)

	got := excuter.segment(context.Background(), testMessages())

	if len(got) != 1 || got[0].Tag != string(store.TagOther) {
		t.Fatalf("expected whole-session fallback, got %+v", got)
	}
}

func TestNormalizeMemoryTagRejectsUnknown(t *testing.T) {
	if got := store.NormalizeMemoryTag("after-sales"); got != store.TagOther {
		t.Fatalf("expected unknown tag to become other, got %q", got)
	}
	if got := store.NormalizeMemoryTag("tech"); got != store.TagTech {
		t.Fatalf("expected tech to be kept, got %q", got)
	}
}
