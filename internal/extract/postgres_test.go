//go:build integration

package extract

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/llm"
	"recallweave/internal/types"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Run with -tags integration and an explicit test-only PostgreSQL URL. Every
// test owns a random schema; the database name must be recallweave_extract_test.
func extractionPostgres(t *testing.T) (context.Context, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("RECALLWEAVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RECALLWEAVE_TEST_DATABASE_URL to run PostgreSQL extraction tests")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || endpoint.Path != "/recallweave_extract_test" {
		t.Fatal("RECALLWEAVE_TEST_DATABASE_URL must be a PostgreSQL URL for recallweave_extract_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), config)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var schema string
	var scoped *gorm.DB
	t.Cleanup(func() {
		cancel()
		if scoped != nil {
			connection, err := scoped.DB()
			if err == nil {
				_ = connection.Close()
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if schema != "" {
			if err := admin.WithContext(cleanupCtx).Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error; err != nil {
				t.Errorf("clean up extraction test schema: %v", err)
			}
		}
		_ = adminSQL.Close()
	})
	if err := admin.WithContext(ctx).Exec("CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public").Error; err != nil {
		t.Fatal(err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	schema = fmt.Sprintf("recallweave_extract_%x", nonce)
	if err := admin.WithContext(ctx).Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	query := endpoint.Query()
	query.Set("search_path", schema+",public")
	query.Set("application_name", schema)
	endpoint.RawQuery = query.Encode()
	scoped, err = gorm.Open(postgres.Open(endpoint.String()), config)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := scoped.DB()
	if err != nil {
		t.Fatal(err)
	}
	connection.SetMaxOpenConns(8)
	if err := scoped.WithContext(ctx).AutoMigrate(&types.Conversation{}, &types.Session{}, &types.Message{}, &types.Memory{}); err != nil {
		t.Fatal(err)
	}
	return ctx, scoped, schema
}

func postgresExtractionSession(t *testing.T, ctx context.Context, db *gorm.DB) uint {
	t.Helper()
	session := types.Session{SourceTag: types.SourceChat, Name: "concurrent extraction", ExtractStatus: types.ExtractPending}
	if err := db.WithContext(ctx).Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	appendPostgresMessages(t, ctx, db, session.ID, 1, 2)
	return session.ID
}

func appendPostgresMessages(t *testing.T, ctx context.Context, db *gorm.DB, sessionID uint, start, end int) {
	t.Helper()
	for seq := start; seq <= end; seq++ {
		message := types.Message{SourceTag: types.SourceChat, SessionID: sessionID, Seq: seq, Role: "user", Content: fmt.Sprintf("message %d", seq)}
		if err := db.WithContext(ctx).Create(&message).Error; err != nil {
			t.Fatal(err)
		}
	}
}

type postgresSegmenter func(context.Context, []llm.Message) ([]llm.Segment, error)

func (segment postgresSegmenter) Segment(ctx context.Context, messages []llm.Message, _, _ []string) ([]llm.Segment, error) {
	return segment(ctx, messages)
}

type postgresEmbedder struct{}

func (postgresEmbedder) Embed(context.Context, string) ([]float64, error) {
	vector := make([]float64, types.EmbeddingDim)
	vector[0] = 1
	return vector, nil
}

type postgresExtractionResult struct {
	count int
	err   error
}

func waitPostgresExtraction(t *testing.T, ctx context.Context, results <-chan postgresExtractionResult) int {
	t.Helper()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.count
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return 0
	}
}

func TestPostgresConcurrentIncrementalExtractionLocksEmptySession(t *testing.T) {
	ctx, db, application := extractionPostgres(t)
	sessionID := postgresExtractionSession(t, ctx, db)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	segmenter := postgresSegmenter(func(ctx context.Context, messages []llm.Message) ([]llm.Segment, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []llm.Segment{{SeqStart: messages[0].Seq, SeqEnd: messages[len(messages)-1].Seq, Title: "shared interval", Summary: "one memory", Tag: "tech"}}, nil
	})
	locker := db.WithContext(ctx).Begin()
	if locker.Error != nil {
		t.Fatal(locker.Error)
	}
	t.Cleanup(func() { _ = locker.Rollback().Error })
	var locked types.Session
	if err := locker.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, sessionID).Error; err != nil {
		t.Fatal(err)
	}
	results := make(chan postgresExtractionResult, 2)
	for i := 0; i < 2; i++ {
		executor := NewExtractExcuter(db, segmenter, postgresEmbedder{})
		go func() {
			count, err := executor.ExtractIncrementalToMemory(ctx, sessionID)
			results <- postgresExtractionResult{count, err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("two independent executors could not generate outside the session transaction")
		}
	}
	close(release)
	// Force both commits to contend on the actual PostgreSQL session row. An
	// empty session has no existing memory rows that could provide this lock.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int64
		if err := db.WithContext(ctx).Raw("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND application_name = ? AND wait_event_type = 'Lock'", application).Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("both extraction commits did not wait for the session row lock")
		}
	}
	if err := locker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	counts := []int{waitPostgresExtraction(t, ctx, results), waitPostgresExtraction(t, ctx, results)}
	sort.Ints(counts)
	if counts[0] != 0 || counts[1] != 1 {
		t.Fatalf("expected one insert and one covered-range skip, got %v", counts)
	}
	var memories []types.Memory
	if err := db.WithContext(ctx).Where("session_id = ?", sessionID).Find(&memories).Error; err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].SeqStart != 1 || memories[0].SeqEnd != 2 {
		t.Fatalf("concurrent extraction duplicated the interval: %+v", memories)
	}
}

func TestPostgresFullExtractionRetainsNewerIncrementalCoverage(t *testing.T) {
	ctx, db, _ := extractionPostgres(t)
	sessionID := postgresExtractionSession(t, ctx, db)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	segmenter := postgresSegmenter(func(ctx context.Context, messages []llm.Message) ([]llm.Segment, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		last := messages[len(messages)-1].Seq
		return []llm.Segment{{SeqStart: messages[0].Seq, SeqEnd: last, Title: "full extraction", Summary: fmt.Sprintf("full-through-%d", last), Tag: "tech"}}, nil
	})
	full := NewExtractExcuter(db, segmenter, postgresEmbedder{})
	results := make(chan postgresExtractionResult, 1)
	go func() {
		count, err := full.ExtractSessionToMemory(ctx, sessionID)
		results <- postgresExtractionResult{count, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	appendPostgresMessages(t, ctx, db, sessionID, 3, 4)
	if count, err := NewExtractExcuter(db, nil, postgresEmbedder{}).ExtractIncrementalToMemory(ctx, sessionID); err != nil || count != 1 {
		t.Fatalf("incremental extraction during full generation: count=%d err=%v", count, err)
	}
	close(release)
	if count := waitPostgresExtraction(t, ctx, results); count != 1 || calls.Load() != 2 {
		t.Fatalf("full extraction did not retry its stale snapshot: count=%d model calls=%d", count, calls.Load())
	}
	var memories []types.Memory
	if err := db.WithContext(ctx).Where("session_id = ?", sessionID).Find(&memories).Error; err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].SeqStart != 1 || memories[0].SeqEnd != 4 || !strings.Contains(memories[0].Content, "full-through-4") {
		t.Fatalf("stale full extraction lost newer messages: %+v", memories)
	}
}
