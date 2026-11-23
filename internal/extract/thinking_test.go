package extract

import (
	"context"
	"testing"

	"recallweave/internal/llm"
	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestThinkingTurnsKeepOnlyVerbatimUserMemories(t *testing.T) {
	messages := []types.Message{
		{Seq: 1, Role: "user", Content: "普通提问"},
		{Seq: 2, Role: "assistant", Content: "普通回答"},
		{Seq: 3, Role: "user", Content: "可以", Thinking: true},
		{Seq: 4, Role: "assistant", Content: "我猜有人反驳你才会改变看法。", Thinking: true},
		{Seq: 5, Role: "user", Content: "这个解释很牵强。\n我写给自己也可以。", Thinking: true},
		{Seq: 6, Role: "assistant", Content: "那我再提一个可能。", Thinking: true},
		{Seq: 7, Role: "user", Content: "另一条普通提问"},
	}
	for _, name := range []string{"model", "fallback"} {
		t.Run(name, func(t *testing.T) {
			recorder := &recordingSegmenter{}
			var model llm.Segmenter
			if name == "model" {
				model = recorder
			}
			memories := NewExtractExcuter(nil, model, nil).generateMemoryFromMessage(context.Background(), 12, messages)
			if len(memories) != 4 || memories[0].SeqStart != 1 || memories[0].SeqEnd != 2 || memories[3].SeqStart != 7 || memories[3].SeqEnd != 7 {
				t.Fatalf("normal memories crossed a thinking turn: %+v", memories)
			}
			for i, msg := range []types.Message{messages[2], messages[4]} {
				memory := memories[i+1]
				if memory.SessionID != 12 || memory.SeqStart != msg.Seq || memory.SeqEnd != msg.Seq || memory.Content != msg.Content || memory.ContentType != types.ContentOther {
					t.Fatalf("user thought was rewritten, classified, or attributed to another message: %+v", memory)
				}
			}
			if name == "model" && (len(recorder.calls) != 2 || len(recorder.calls[0]) != 2 || len(recorder.calls[1]) != 1 || recorder.calls[1][0].Seq != 7) {
				t.Fatalf("unexpected messages sent to summarization: %+v", recorder.calls)
			}
		})
	}
}

func TestThinkingNavigationDoesNotBecomeAUserMemory(t *testing.T) {
	checkpoint := &types.ThinkingCheckpoint{Text: "以前的问题", Author: "user"}
	source := &types.ThoughtSource{SessionID: 9, SeqStart: 1, SeqEnd: 2}
	messages := []types.Message{
		{Seq: 1, Role: "user", Content: "接着我留下的思考继续。", Thinking: true, ThinkingContext: &types.ThinkingContext{Navigation: true, Checkpoint: checkpoint}},
		{Seq: 2, Role: "assistant", Content: "这是我的猜测。", Thinking: true},
		{Seq: 3, Role: "user", Content: "从我选中的会话 9 消息 1–2 接着想。", Thinking: true, ThinkingContext: &types.ThinkingContext{Navigation: true, Source: source}},
		{Seq: 4, Role: "assistant", Content: "再换一个角度。", Thinking: true},
		{Seq: 5, Role: "user", Content: "我会改变想法，因为写出来才看见矛盾。", Thinking: true, ThinkingContext: &types.ThinkingContext{Source: source, Checkpoint: checkpoint}},
	}
	recorder := &recordingSegmenter{}
	memories := NewExtractExcuter(nil, recorder, nil).generateMemoryFromMessage(context.Background(), 7, messages)
	if len(recorder.calls) != 0 || len(memories) != 1 || memories[0].SeqStart != 5 || memories[0].SeqEnd != 5 || memories[0].Content != messages[4].Content {
		t.Fatalf("expected only the user's typed thought, got %+v (model calls %+v)", memories, recorder.calls)
	}
}

func TestIncrementalExtractionSkipsThinkingReplyOnRetryAndOnNextTurn(t *testing.T) {
	const sessionID uint = 19
	db, mock := extractionDatabase(t)
	expectTail := func(hasNewMessage bool) {
		expectExtractionScope(mock, sessionID)
		expectCoveredSequence(mock, sessionID, 3)
		rows := sqlmock.NewRows([]string{"seq", "role", "content", "thinking"}).
			AddRow(4, "assistant", "我有一个新的猜想", true)
		if hasNewMessage {
			rows.AddRow(5, "user", "later ordinary question", false)
		}
		mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*AND seq > .*ORDER BY seq`).
			WithArgs(sessionID, 3).WillReturnRows(rows)
	}
	expectTail(false)
	expectExtractionDone(mock, sessionID)
	expectTail(true)
	expectIncrementalCommit(mock, sessionID, 3, 5, 5, "ordinary summary")
	expectExtractionDone(mock, sessionID)

	model := concurrentSegmenter(func(_ context.Context, input []llm.Message) ([]llm.Segment, error) {
		if len(input) != 1 || input[0].Seq != 5 {
			t.Fatalf("included assistant conjecture in a later batch: %+v", input)
		}
		return summarySegment(input, "ordinary summary"), nil
	})
	extractor := NewExtractExcuter(db, model, nil)
	for _, wantCount := range []int{0, 1} {
		count, err := extractor.ExtractIncrementalToMemory(context.Background(), sessionID)
		if err != nil || count != wantCount {
			t.Fatalf("added %d memories, expected %d: %v", count, wantCount, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
