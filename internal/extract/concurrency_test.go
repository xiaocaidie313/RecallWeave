package extract

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/llm"
	"recallweave/internal/types"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests control concurrent model calls and SQL observations. sqlmock
// verifies the persistence protocol; it does not emulate PostgreSQL row locks.
type concurrentSegmenter func(context.Context, []llm.Message) ([]llm.Segment, error)

func (f concurrentSegmenter) Segment(ctx context.Context, messages []llm.Message, _, _ []string) ([]llm.Segment, error) {
	return f(ctx, messages)
}

func extractionDatabase(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db, mock
}

func expectExtractionScope(mock sqlmock.Sqlmock, sessionID uint) {
	mock.ExpectQuery(scopeQuery).WithArgs(sessionID, 1).
		WillReturnRows(scopeRows().AddRow(sessionID, nil, nil, nil))
}

func expectCoveredSequence(mock sqlmock.Sqlmock, sessionID uint, covered int) {
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(seq_end\), 0\) AS seq FROM "memories" WHERE session_id =`).WithArgs(sessionID).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(covered))
}

func extractionMessageRows(sessionID uint, sequences ...int) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"session_id", "seq", "role", "content"})
	for _, seq := range sequences {
		rows.AddRow(sessionID, seq, "user", fmt.Sprintf("message %d", seq))
	}
	return rows
}

func expectUncoveredMessages(mock sqlmock.Sqlmock, sessionID uint, covered int, sequences ...int) {
	expectCoveredSequence(mock, sessionID, covered)
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*AND seq > .*ORDER BY seq`).WithArgs(sessionID, covered).
		WillReturnRows(extractionMessageRows(sessionID, sequences...))
}

func expectSessionWriteLock(mock sqlmock.Sqlmock, sessionID uint) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE id = .*FOR UPDATE`).WithArgs(sessionID, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(sessionID))
}

func expectMemoryInsert(mock sqlmock.Sqlmock, sessionID uint, start, end int, summary string) {
	mock.ExpectQuery(`INSERT INTO "memories"`).
		WithArgs(sqlmock.AnyArg(), sessionID, start, end, "memory", summary, types.TagTech, types.ContentOther, types.MemoryActive,
			nil, "", 0, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(end))
}

func expectIncrementalCommit(mock sqlmock.Sqlmock, sessionID uint, covered, start, end int, summary string) {
	expectSessionWriteLock(mock, sessionID)
	expectCoveredSequence(mock, sessionID, covered)
	expectMemoryInsert(mock, sessionID, start, end, summary)
	mock.ExpectCommit()
}

func expectExtractionDone(mock sqlmock.Sqlmock, sessionID uint) {
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "sessions" SET `).
		WithArgs("", types.ExtractDone, sqlmock.AnyArg(), sqlmock.AnyArg(), sessionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func summarySegment(messages []llm.Message, summary string) []llm.Segment {
	return []llm.Segment{{SeqStart: messages[0].Seq, SeqEnd: messages[len(messages)-1].Seq, Title: "memory", Summary: summary, Tag: "tech"}}
}

type extractionResult struct {
	count int
	err   error
}

func startIncremental(ctx context.Context, extractor *ExtractExcuter, sessionID uint) <-chan extractionResult {
	result := make(chan extractionResult, 1)
	go func() {
		count, err := extractor.ExtractIncrementalToMemory(ctx, sessionID)
		result <- extractionResult{count, err}
	}()
	return result
}

func waitForExtraction(t *testing.T, results <-chan extractionResult, count int, wantErr error) {
	t.Helper()
	select {
	case result := <-results:
		if result.count != count || !errors.Is(result.err, wantErr) {
			t.Fatalf("extraction result: count=%d err=%v; want count=%d err=%v", result.count, result.err, count, wantErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("extraction did not finish")
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("extraction did not reach the expected barrier")
	}
}

// Observing Done lets the test know the second call has begun waiting, without
// sleeping or relying on the gate's internal fields.
type observedWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestConcurrentIncrementalRequestsReuseCommittedCoverage(t *testing.T) {
	for _, appendNewMessages := range []bool{false, true} {
		t.Run(fmt.Sprintf("new_messages=%v", appendNewMessages), func(t *testing.T) {
			db, mock := extractionDatabase(t)
			expectExtractionScope(mock, 10)
			expectUncoveredMessages(mock, 10, 0, 1, 2)
			expectIncrementalCommit(mock, 10, 0, 1, 2, "first pair")
			expectExtractionDone(mock, 10)
			expectExtractionScope(mock, 10)
			if appendNewMessages {
				expectUncoveredMessages(mock, 10, 2, 3, 4)
				expectIncrementalCommit(mock, 10, 2, 3, 4, "second pair")
			} else {
				expectUncoveredMessages(mock, 10, 2)
			}
			expectExtractionDone(mock, 10)

			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			model := concurrentSegmenter(func(ctx context.Context, messages []llm.Message) ([]llm.Segment, error) {
				if calls.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return summarySegment(messages, "first pair"), nil
				}
				if messages[0].Seq != 3 || messages[len(messages)-1].Seq != 4 {
					return nil, fmt.Errorf("second model call reused already-covered messages: %+v", messages)
				}
				return summarySegment(messages, "second pair"), nil
			})
			extractor := NewExtractExcuter(db, model, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			first := startIncremental(ctx, extractor, 10)
			waitForSignal(t, entered)
			waiting := &observedWaitContext{Context: ctx, observed: make(chan struct{})}
			second := startIncremental(waiting, extractor, 10)
			waitForSignal(t, waiting.observed)
			close(release)
			waitForExtraction(t, first, 1, nil)
			wantCalls, wantSecondCount := int32(1), 0
			if appendNewMessages {
				wantCalls, wantSecondCount = 2, 1
			}
			waitForExtraction(t, second, wantSecondCount, nil)
			if calls.Load() != wantCalls {
				t.Fatalf("generated duplicate summaries: model calls=%d want=%d", calls.Load(), wantCalls)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDifferentExtractorsRegenerateOnlyUncoveredSuffixAfterConflict(t *testing.T) {
	db, mock := extractionDatabase(t)
	expectExtractionScope(mock, 10)
	expectUncoveredMessages(mock, 10, 0, 1, 2)
	expectExtractionScope(mock, 10)
	expectUncoveredMessages(mock, 10, 0, 1, 2, 3, 4)
	expectIncrementalCommit(mock, 10, 0, 1, 2, "first pair")
	expectExtractionDone(mock, 10)
	// Another executor committed while the second model was running. Its
	// original 1..4 summary must be discarded rather than relabeled as 3..4.
	expectSessionWriteLock(mock, 10)
	expectCoveredSequence(mock, 10, 2)
	mock.ExpectCommit()
	expectUncoveredMessages(mock, 10, 2, 3, 4)
	expectIncrementalCommit(mock, 10, 2, 3, 4, "fresh suffix")
	expectExtractionDone(mock, 10)

	firstEntered, firstRelease := make(chan struct{}), make(chan struct{})
	secondEntered, secondRelease := make(chan struct{}), make(chan struct{})
	firstModel := concurrentSegmenter(func(ctx context.Context, messages []llm.Message) ([]llm.Segment, error) {
		close(firstEntered)
		select {
		case <-firstRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return summarySegment(messages, "first pair"), nil
	})
	var secondInputs [][]int
	secondModel := concurrentSegmenter(func(ctx context.Context, messages []llm.Message) ([]llm.Segment, error) {
		sequences := make([]int, len(messages))
		for i, message := range messages {
			sequences[i] = message.Seq
		}
		secondInputs = append(secondInputs, sequences)
		if len(secondInputs) == 1 {
			close(secondEntered)
			select {
			case <-secondRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return summarySegment(messages, "stale whole range"), nil
		}
		return summarySegment(messages, "fresh suffix"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := startIncremental(ctx, NewExtractExcuter(db, firstModel, nil), 10)
	waitForSignal(t, firstEntered)
	second := startIncremental(ctx, NewExtractExcuter(db, secondModel, nil), 10)
	waitForSignal(t, secondEntered)
	close(firstRelease)
	waitForExtraction(t, first, 1, nil)
	close(secondRelease)
	waitForExtraction(t, second, 1, nil)
	if want := [][]int{{1, 2, 3, 4}, {3, 4}}; !reflect.DeepEqual(secondInputs, want) {
		t.Fatalf("conflict did not regenerate the uncovered suffix: got %v want %v", secondInputs, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledQueuedExtractionDoesNotReadOrGenerate(t *testing.T) {
	for _, mode := range []string{"incremental", "full"} {
		t.Run(mode, func(t *testing.T) {
			db, mock := extractionDatabase(t)
			expectExtractionScope(mock, 10)
			expectUncoveredMessages(mock, 10, 0, 1, 2)
			expectIncrementalCommit(mock, 10, 0, 1, 2, "first pair")
			expectExtractionDone(mock, 10)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			model := concurrentSegmenter(func(ctx context.Context, messages []llm.Message) ([]llm.Segment, error) {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return summarySegment(messages, "first pair"), nil
			})
			extractor := NewExtractExcuter(db, model, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			first := startIncremental(ctx, extractor, 10)
			waitForSignal(t, entered)
			queuedCtx, cancelQueued := context.WithCancel(ctx)
			defer cancelQueued()
			waiting := &observedWaitContext{Context: queuedCtx, observed: make(chan struct{})}
			var second <-chan extractionResult
			if mode == "full" {
				result := make(chan extractionResult, 1)
				go func() {
					count, err := extractor.ExtractSessionToMemory(waiting, 10)
					result <- extractionResult{count, err}
				}()
				second = result
			} else {
				second = startIncremental(waiting, extractor, 10)
			}
			waitForSignal(t, waiting.observed)
			cancelQueued()
			waitForExtraction(t, second, 0, context.Canceled)
			close(release)
			waitForExtraction(t, first, 1, nil)
			if calls.Load() != 1 {
				t.Fatalf("cancelled waiter called the model: %d calls", calls.Load())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCancellationDuringGenerationDoesNotPersistFallbackMemory(t *testing.T) {
	db, mock := extractionDatabase(t)
	expectExtractionScope(mock, 10)
	expectUncoveredMessages(mock, 10, 0, 1, 2)
	entered := make(chan struct{})
	model := concurrentSegmenter(func(ctx context.Context, _ []llm.Message) ([]llm.Segment, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := startIncremental(ctx, NewExtractExcuter(db, model, nil), 10)
	waitForSignal(t, entered)
	cancel()
	waitForExtraction(t, result, 0, context.Canceled)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFullExtractionRetriesSnapshotOlderThanStoredCoverage(t *testing.T) {
	db, mock := extractionDatabase(t)
	expectExtractionScope(mock, 10)
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*ORDER BY seq`).WithArgs(10).
		WillReturnRows(extractionMessageRows(10, 1, 2))
	expectSessionWriteLock(mock, 10)
	mock.ExpectQuery(`SELECT .* FROM "memories".*FOR UPDATE`).WithArgs(10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "session_id", "seq_end", "status"}).AddRow(1, 10, 4, types.MemoryActive))
	mock.ExpectCommit()
	// The first transaction cannot delete newer coverage. Retry with current
	// source messages before generating the replacement.
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = .*ORDER BY seq`).WithArgs(10).
		WillReturnRows(extractionMessageRows(10, 1, 2, 3, 4))
	expectSessionWriteLock(mock, 10)
	mock.ExpectQuery(`SELECT .* FROM "memories".*FOR UPDATE`).WithArgs(10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "session_id", "seq_end", "status"}).AddRow(1, 10, 4, types.MemoryActive))
	mock.ExpectExec(`DELETE FROM "memories" WHERE session_id =`).WithArgs(10).WillReturnResult(sqlmock.NewResult(0, 1))
	expectMemoryInsert(mock, 10, 1, 4, "fresh full snapshot")
	mock.ExpectCommit()
	expectExtractionDone(mock, 10)
	var inputs [][]int
	model := concurrentSegmenter(func(_ context.Context, messages []llm.Message) ([]llm.Segment, error) {
		sequences := make([]int, len(messages))
		for i, message := range messages {
			sequences[i] = message.Seq
		}
		inputs = append(inputs, sequences)
		if len(inputs) == 1 {
			return summarySegment(messages, "stale full snapshot"), nil
		}
		return summarySegment(messages, "fresh full snapshot"), nil
	})
	count, err := NewExtractExcuter(db, model, nil).ExtractSessionToMemory(context.Background(), 10)
	if count != 1 || err != nil {
		t.Fatalf("full extraction failed: count=%d err=%v", count, err)
	}
	if want := [][]int{{1, 2}, {1, 2, 3, 4}}; !reflect.DeepEqual(inputs, want) {
		t.Fatalf("stale snapshot was not regenerated: got %v want %v", inputs, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalCommitRejectsCoverageMovingBackward(t *testing.T) {
	db, mock := extractionDatabase(t)
	// A full re-extraction may replace 1..4 with a differently segmented range.
	// A candidate generated from >4 is stale even if the new maximum is lower.
	expectSessionWriteLock(mock, 10)
	expectCoveredSequence(mock, 10, 2)
	mock.ExpectCommit()
	memories := []types.Memory{{SessionID: 10, SeqStart: 5, SeqEnd: 6, Title: "stale suffix", Content: "must not be inserted"}}
	appended, err := NewExtractExcuter(db, nil, nil).appendMemories(context.Background(), 10, 4, memories)
	if appended || err != nil {
		t.Fatalf("changed coverage must cause a retry: appended=%v err=%v", appended, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalInsertFailureRollsBackAndDoesNotReportSuccess(t *testing.T) {
	db, mock := extractionDatabase(t)
	expectSessionWriteLock(mock, 10)
	expectCoveredSequence(mock, 10, 0)
	insertErr := errors.New("database insert unavailable")
	mock.ExpectQuery(`INSERT INTO "memories"`).WillReturnError(insertErr)
	mock.ExpectRollback()
	memories := []types.Memory{{SessionID: 10, SeqStart: 1, SeqEnd: 2, Title: "memory", Content: "candidate"}}
	appended, err := NewExtractExcuter(db, nil, nil).appendMemories(context.Background(), 10, 0, memories)
	if appended || !errors.Is(err, insertErr) {
		t.Fatalf("failed insert reported success: appended=%v err=%v", appended, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPerpetualCoverageConflictsStopAndMarkExtractionFailed(t *testing.T) {
	db, mock := extractionDatabase(t)
	expectExtractionScope(mock, 10)
	for attempt := 0; attempt < maxExtractionAttempts; attempt++ {
		covered := attempt * 2
		expectUncoveredMessages(mock, 10, covered, covered+1, covered+2)
		expectSessionWriteLock(mock, 10)
		// Each generated candidate loses to another writer before commit. There
		// must be no INSERT from this executor and no unbounded model retries.
		expectCoveredSequence(mock, 10, covered+2)
		mock.ExpectCommit()
	}
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "sessions" SET `).
		WithArgs(errExtractionConflict.Error(), types.ExtractFailed, sqlmock.AnyArg(), 10).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	calls := 0
	model := concurrentSegmenter(func(_ context.Context, messages []llm.Message) ([]llm.Segment, error) {
		calls++
		return summarySegment(messages, "uncommitted candidate"), nil
	})
	count, err := NewExtractExcuter(db, model, nil).ExtractIncrementalToMemory(context.Background(), 10)
	if count != 0 || !errors.Is(err, errExtractionConflict) || calls != maxExtractionAttempts {
		t.Fatalf("conflict retry was not bounded: count=%d err=%v model_calls=%d", count, err, calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
