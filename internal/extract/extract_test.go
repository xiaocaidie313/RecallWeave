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

func (f fakeSegmenter) Segment(context.Context, []llm.Message, []string, []string) ([]llm.Segment, error) {
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

func numberedMessages(n int) []store.Message {
	messages := make([]store.Message, 0, n)
	for i := 1; i <= n; i++ {
		messages = append(messages, store.Message{
			SessionID: 1,
			Seq:       i,
			Role:      "user",
			Content:   "消息内容",
		})
	}
	return messages
}

type recordingSegmenter struct {
	calls [][]llm.Message
	errOn int
}

func (r *recordingSegmenter) Segment(_ context.Context, messages []llm.Message, _, _ []string) ([]llm.Segment, error) {
	copied := append([]llm.Message(nil), messages...)
	r.calls = append(r.calls, copied)
	if r.errOn > 0 && len(r.calls) == r.errOn {
		return nil, errors.New("boom")
	}
	return []llm.Segment{{
		SeqStart: messages[0].Seq,
		SeqEnd:   messages[len(messages)-1].Seq,
		Title:    "批",
		Summary:  "摘要",
		Tag:      "tech",
	}}, nil
}

func TestSegmentSplitsLongSessions(t *testing.T) {
	recorder := &recordingSegmenter{}
	excuter := NewExtractExcuter(nil, recorder, nil)

	got := excuter.segment(context.Background(), numberedMessages(40))
	if len(recorder.calls) < 2 {
		t.Fatalf("expected multiple batches, got %d calls", len(recorder.calls))
	}
	if len(recorder.calls[0]) > segmentBatchMaxMessages {
		t.Fatalf("first batch too large: %d", len(recorder.calls[0]))
	}
	if len(got) != len(recorder.calls) {
		t.Fatalf("expected one segment per batch, got %d segments from %d batches", len(got), len(recorder.calls))
	}

	firstEnd := recorder.calls[0][len(recorder.calls[0])-1].Seq
	secondStart := recorder.calls[1][0].Seq
	if firstEnd-secondStart+1 != segmentBatchOverlap {
		t.Fatalf("expected overlap of %d, firstEnd=%d secondStart=%d", segmentBatchOverlap, firstEnd, secondStart)
	}
	for _, segment := range got {
		if segment.SeqStart < 1 || segment.SeqEnd > 40 {
			t.Fatalf("segment out of range: %+v", segment)
		}
	}
}

func TestSegmentFallsBackPerBatch(t *testing.T) {
	recorder := &recordingSegmenter{errOn: 2}
	excuter := NewExtractExcuter(nil, recorder, nil)

	got := excuter.segment(context.Background(), numberedMessages(40))
	if len(got) < 2 {
		t.Fatalf("expected remaining batches after one failure, got %d", len(got))
	}

	var fallback llm.Segment
	found := false
	for _, segment := range got {
		if segment.Tag == string(store.TagOther) {
			fallback = segment
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected one batch fallback, got %+v", got)
	}
	if fallback.SeqStart == 1 && fallback.SeqEnd == 40 {
		t.Fatal("a single failed batch should not cover the whole session")
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

func TestMemoriesFromPreservesContentTypeAndOriginalRange(t *testing.T) {
	executer := NewExtractExcuter(nil, fakeSegmenter{segments: []llm.Segment{
		{SeqStart: 1, SeqEnd: 2, Title: "暂缓", Summary: "先暂缓 multi-agent", Tag: "tech", ContentType: "decision"},
		{SeqStart: 3, SeqEnd: 3, Title: "开始", Summary: "决定开始做 multi-agent", Tag: "tech", ContentType: "decision"},
	}}, nil)
	got := executer.generateMemoryFromMessage(context.Background(), 7, testMessages())
	if len(got) != 2 || got[0].ContentType != store.ContentDecision || got[1].ContentType != store.ContentDecision {
		t.Fatalf("decisions should remain separate typed memories: %+v", got)
	}
	if got[0].SessionID != 7 || got[0].SeqStart != 1 || got[0].SeqEnd != 2 || got[1].SeqStart != 3 {
		t.Fatalf("source ranges changed: %+v", got)
	}
}

func TestMemoriesFromNormalizesContentTypes(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want store.ContentType
	}{
		{"fact", store.ContentFact},
		{"preference", store.ContentPreference},
		{"decision", store.ContentDecision},
		{"task", store.ContentTask},
		{"question", store.ContentQuestion},
		{"", store.ContentOther},
		{"unsupported", store.ContentOther},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			executer := NewExtractExcuter(nil, fakeSegmenter{segments: []llm.Segment{{
				SeqStart: 1, SeqEnd: 3, Title: "记忆", Summary: "摘要", ContentType: tc.raw,
			}}}, nil)
			got := executer.generateMemoryFromMessage(context.Background(), 1, testMessages())
			if len(got) != 1 || got[0].ContentType != tc.want {
				t.Fatalf("content_type %q: %+v", tc.raw, got)
			}
		})
	}
}

func TestMemoriesFromFallbackHasUnknownContentType(t *testing.T) {
	for _, segmenter := range []llm.Segmenter{nil, fakeSegmenter{err: errors.New("unavailable")}} {
		got := NewExtractExcuter(nil, segmenter, nil).generateMemoryFromMessage(context.Background(), 1, testMessages())
		if len(got) != 1 || got[0].ContentType != store.ContentOther {
			t.Fatalf("fallback must not invent a classification: %+v", got)
		}
	}
}
