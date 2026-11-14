//go:build integration

package ingest_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appdb "recallweave/internal/db"
	"recallweave/internal/extract"
	"recallweave/internal/ingest"
	"recallweave/internal/llm"
	appmcp "recallweave/internal/mcp"
	"recallweave/internal/memory"
	"recallweave/internal/session"
	"recallweave/internal/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func transcriptPostgres(t *testing.T) (context.Context, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("RECALLWEAVE_TRANSCRIPT_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RECALLWEAVE_TRANSCRIPT_DATABASE_URL for transcript integration tests")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || endpoint.Path != "/recallweave_transcript_test" {
		t.Fatal("transcript tests require a PostgreSQL URL for recallweave_transcript_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal(err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminSQL.Close() })
	if err := admin.WithContext(ctx).Exec("CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public").Error; err != nil {
		t.Fatal(err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("transcript_%x", nonce)
	if err := admin.WithContext(ctx).Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Error(err)
		}
	})
	query := endpoint.Query()
	query.Set("search_path", schema+",public")
	endpoint.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(endpoint.String()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := appdb.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	return ctx, db
}

type transcriptModel struct{ calls atomic.Int32 }

func (m *transcriptModel) Segment(_ context.Context, messages []llm.Message, _, _ []string) ([]llm.Segment, error) {
	m.calls.Add(1)
	return []llm.Segment{{
		SeqStart: messages[0].Seq, SeqEnd: messages[len(messages)-1].Seq,
		Title: "transcript evidence", Summary: messages[0].Content, Tag: "tech", ContentType: "fact",
	}}, nil
}

func (*transcriptModel) Embed(context.Context, string) ([]float64, error) {
	values := make([]float64, types.EmbeddingDim)
	values[0] = 1
	return values, nil
}

func transcriptMessages(count int) []ingest.TranscriptMessage {
	messages := make([]ingest.TranscriptMessage, count)
	for i := range messages {
		role := "user"
		if i%2 != 0 {
			role = "assistant"
		}
		messages[i] = ingest.TranscriptMessage{Role: role, Content: fmt.Sprintf("  original message %d\nnext line  ", i+1)}
	}
	return messages
}

func transcriptClient(t *testing.T, ctx context.Context, db *gorm.DB, writer *ingest.Service) *mcp.ClientSession {
	t.Helper()
	server := appmcp.NewMcpMemoryReaderServer(memory.NewMemoryManger(db, nil), session.NewSessionManager(db))
	if writer != nil {
		appmcp.RegisterImportTranscript(server, writer)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "transcript-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type transcriptReceipt struct {
	SessionID     uint   `json:"session_id"`
	MessageCount  int    `json:"message_count"`
	AddedMessages int    `json:"added_messages"`
	ExtractStatus string `json:"extract_status"`
}

func importViaMCP(t *testing.T, ctx context.Context, client *mcp.ClientSession, messages []ingest.TranscriptMessage, externalID string, seqStart int) transcriptReceipt {
	t.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "import_transcript", Arguments: map[string]any{
		"source_tag": "codex", "name": "sync test", "messages": messages,
		"external_session_id": externalID, "seq_start": seqStart,
	}})
	if err != nil || result.IsError {
		t.Fatalf("MCP import failed: %+v %v", result, err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var receipt transcriptReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestPostgresTranscriptSyncOverMCP(t *testing.T) {
	ctx, db := transcriptPostgres(t)
	model := &transcriptModel{}
	writer := transcriptClient(t, ctx, db, ingest.NewService(db, extract.NewExtractExcuter(db, model, model)))
	messages := transcriptMessages(15)
	first := importViaMCP(t, ctx, writer, messages[:10], "thread", 1)
	if first.AddedMessages != 10 || first.MessageCount != 10 || first.ExtractStatus != types.ExtractDone {
		t.Fatalf("initial receipt: %+v", first)
	}
	var before []types.Message
	if err := db.Where("session_id = ?", first.SessionID).Order("seq").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	var originalMemory types.Memory
	if err := db.Where("session_id = ?", first.SessionID).Take(&originalMemory).Error; err != nil {
		t.Fatal(err)
	}
	second := importViaMCP(t, ctx, writer, messages, "thread", 1)
	retry := importViaMCP(t, ctx, writer, messages, "thread", 1)
	if second.SessionID != first.SessionID || second.AddedMessages != 5 || second.MessageCount != 15 ||
		retry.SessionID != first.SessionID || retry.AddedMessages != 0 || retry.MessageCount != 15 || model.calls.Load() != 2 {
		t.Fatalf("append/retry receipts: %+v %+v; model calls %d", second, retry, model.calls.Load())
	}
	var after []types.Message
	if err := db.Where("session_id = ?", first.SessionID).Order("seq").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	for i, old := range before {
		if after[i].ID != old.ID || after[i].Seq != old.Seq || after[i].Content != old.Content || after[i].Role != old.Role {
			t.Fatalf("saved original changed at message %d", i+1)
		}
	}
	var memories []types.Memory
	if err := db.Where("session_id = ?", first.SessionID).Order("seq_start").Find(&memories).Error; err != nil {
		t.Fatal(err)
	}
	if len(memories) != 2 || memories[0].ID != originalMemory.ID || memories[0].SeqEnd != 10 ||
		memories[1].SeqStart != 11 || memories[1].SeqEnd != 15 {
		t.Fatalf("memory ranges changed or duplicated: %+v", memories)
	}
	// A new service with no extractor still reuses the completed imported session.
	newWriter := transcriptClient(t, ctx, db, ingest.NewService(db, nil))
	stale := importViaMCP(t, ctx, newWriter, messages[:10], "thread", 1)
	if stale.MessageCount != 15 || stale.AddedMessages != 0 || stale.ExtractStatus != types.ExtractDone {
		t.Fatalf("shorter replay truncated or reset the session: %+v", stale)
	}
	reader := transcriptClient(t, ctx, db, nil)
	result, err := reader.CallTool(ctx, &mcp.CallToolParams{Name: "get_messages", Arguments: map[string]any{
		"session_id": first.SessionID, "seq_start": originalMemory.SeqStart, "seq_end": originalMemory.SeqEnd,
	}})
	if err != nil || result.IsError {
		t.Fatalf("original citation cannot be read: %+v %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var read struct{ Messages []ingest.TranscriptMessage }
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Messages) != 10 {
		t.Fatalf("citation returned %d messages", len(read.Messages))
	}
	for i, old := range messages[:10] {
		if read.Messages[i] != old {
			t.Fatalf("citation text or role changed at message %d", i+1)
		}
	}
	var stored types.Session
	if err := db.First(&stored, first.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	var source []ingest.TranscriptMessage
	if err := json.Unmarshal([]byte(stored.RawText), &source); err != nil || len(source) != 15 {
		t.Fatalf("full raw transcript not retained: %v %d", err, len(source))
	}
}

func TestPostgresTranscriptSyncConflictAndRetry(t *testing.T) {
	ctx, db := transcriptPostgres(t)
	service := ingest.NewService(db, nil)
	messages := transcriptMessages(15)
	opt := ingest.TranscriptOptions{ExternalSessionID: "thread"}
	first, err := service.ImportTranscript(ctx, types.SourceCodex, "sync", messages[:10], opt)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]ingest.TranscriptMessage(nil), messages...)
	edited[4].Content = "changed original"
	if _, err := service.ImportTranscript(ctx, types.SourceCodex, "sync", edited, opt); !errors.Is(err, ingest.ErrTranscriptConflict) {
		t.Fatalf("edited original accepted: %v", err)
	}
	edited[4] = messages[4]
	edited[4].Role = "assistant"
	if _, err := service.ImportTranscript(ctx, types.SourceCodex, "sync", edited, opt); !errors.Is(err, ingest.ErrTranscriptConflict) {
		t.Fatalf("changed speaker accepted: %v", err)
	}
	gap := ingest.TranscriptOptions{ExternalSessionID: "new-thread", SeqStart: 2}
	if _, err := service.ImportTranscript(ctx, types.SourceCodex, "gap", messages[:1], gap); !errors.Is(err, ingest.ErrTranscriptGap) {
		t.Fatalf("missing first message accepted: %v", err)
	}
	if err := db.Exec("ALTER TABLE messages ADD CONSTRAINT reject_batch CHECK (content <> 'reject-write')").Error; err != nil {
		t.Fatal(err)
	}
	partial := []ingest.TranscriptMessage{{Role: "user", Content: "would append"}, {Role: "assistant", Content: "reject-write"}}
	if _, err := service.ImportTranscript(ctx, types.SourceCodex, "sync", partial, ingest.TranscriptOptions{
		ExternalSessionID: "thread", SeqStart: 11,
	}); err == nil {
		t.Fatal("database rejection unexpectedly succeeded")
	}
	if err := db.Exec("ALTER TABLE messages DROP CONSTRAINT reject_batch").Error; err != nil {
		t.Fatal(err)
	}
	var sessionCount, sourceCount, messageCount int64
	if err := db.Model(&types.Session{}).Count(&sessionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&types.TranscriptSource{}).Count(&sourceCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&types.Message{}).Count(&messageCount).Error; err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 || sourceCount != 1 || messageCount != 10 {
		t.Fatalf("failed batch partially persisted: sessions=%d sources=%d messages=%d", sessionCount, sourceCount, messageCount)
	}
	// Crossing the per-request limit works by supplying consecutive batches.
	all := transcriptMessages(60)
	next, err := service.ImportTranscript(ctx, types.SourceCodex, "sync", all[10:], ingest.TranscriptOptions{
		ExternalSessionID: "thread", SeqStart: 11,
	})
	if err != nil || next.SessionID != first.SessionID || next.AddedMessages != 50 || next.MessageCount != 60 {
		t.Fatalf("chunk append failed: %+v %v", next, err)
	}
	// An overlapping chunk verifies its prefix and appends only its new tail.
	all = transcriptMessages(65)
	overlap, err := service.ImportTranscript(ctx, types.SourceCodex, "sync", all[55:], ingest.TranscriptOptions{
		ExternalSessionID: "thread", SeqStart: 56,
	})
	if err != nil || overlap.AddedMessages != 5 || overlap.MessageCount != 65 {
		t.Fatalf("overlap duplicated messages: %+v %v", overlap, err)
	}
	other, err := service.ImportTranscript(ctx, types.SourceDeepSeekHarness, "separate source", messages[:1], opt)
	if err != nil || other.SessionID == first.SessionID {
		t.Fatalf("different source platforms shared a session: %+v %v", other, err)
	}
	// Existing clients without an external ID retain their independent imports.
	legacy, err := service.ImportTranscript(ctx, types.SourceCodex, "legacy", messages[:1])
	if err != nil {
		t.Fatal(err)
	}
	legacyAgain, err := service.ImportTranscript(ctx, types.SourceCodex, "legacy", messages[:1])
	if err != nil || legacyAgain.SessionID == legacy.SessionID {
		t.Fatalf("legacy imports unexpectedly merged: %+v %v", legacyAgain, err)
	}
	model := &transcriptModel{}
	withExtraction := ingest.NewService(db, extract.NewExtractExcuter(db, model, model))
	if err := db.Exec("ALTER TABLE memories ADD CONSTRAINT reject_retry CHECK (content <> 'force-retry')").Error; err != nil {
		t.Fatal(err)
	}
	retryOpt := ingest.TranscriptOptions{ExternalSessionID: "retry"}
	retryMessages := []ingest.TranscriptMessage{{Role: "user", Content: "force-retry"}}
	failed, err := withExtraction.ImportTranscript(ctx, types.SourceCodex, "retry", retryMessages, retryOpt)
	if err != nil || failed.ExtractStatus != types.ExtractFailed || failed.AddedMessages != 1 {
		t.Fatalf("failed extraction lost its save receipt: %+v %v", failed, err)
	}
	if err := db.Exec("ALTER TABLE memories DROP CONSTRAINT reject_retry").Error; err != nil {
		t.Fatal(err)
	}
	retried, err := withExtraction.ImportTranscript(ctx, types.SourceCodex, "retry", retryMessages, retryOpt)
	if err != nil || retried.SessionID != failed.SessionID || retried.AddedMessages != 0 ||
		retried.MessageCount != 1 || retried.ExtractStatus != types.ExtractDone {
		t.Fatalf("extraction retry duplicated source or failed: %+v %v", retried, err)
	}
}

func TestPostgresTranscriptSyncConcurrentWriters(t *testing.T) {
	ctx, db := transcriptPostgres(t)
	messages := transcriptMessages(15)
	opt := ingest.TranscriptOptions{ExternalSessionID: "concurrent"}
	var wg sync.WaitGroup
	results := make(chan ingest.ImportResult, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Independent services exercise the database lock, not an in-memory gate.
			result, err := ingest.NewService(db, nil).ImportTranscript(ctx, types.SourceCodex, "concurrent", messages, opt)
			results <- result
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id uint
	added := 0
	for result := range results {
		if id != 0 && result.SessionID != id {
			t.Fatalf("writers created different sessions: %d %d", id, result.SessionID)
		}
		id = result.SessionID
		added += result.AddedMessages
	}
	var count int64
	if err := db.Model(&types.Message{}).Where("session_id = ?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if added != 15 || count != 15 {
		t.Fatalf("concurrent retries duplicated messages: added=%d stored=%d", added, count)
	}
	// Concurrent growing snapshots and stale prefixes must converge on the longest.
	results = make(chan ingest.ImportResult, 8)
	failures = make(chan error, 8)
	messages = transcriptMessages(20)
	for i := 0; i < 8; i++ {
		size := 10 + i%3*5
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := ingest.NewService(db, nil).ImportTranscript(ctx, types.SourceCodex, "concurrent", messages[:size], opt)
			results <- result
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	added = 0
	for result := range results {
		if result.SessionID != id {
			t.Fatalf("append created another session: %+v", result)
		}
		added += result.AddedMessages
	}
	if err := db.Model(&types.Message{}).Where("session_id = ?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if added != 5 || count != 20 {
		t.Fatalf("growing snapshots lost or duplicated messages: added=%d stored=%d", added, count)
	}
}
