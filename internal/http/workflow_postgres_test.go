//go:build integration

package http_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/chat"
	"recallweave/internal/conversation"
	appdb "recallweave/internal/db"
	"recallweave/internal/events"
	"recallweave/internal/extract"
	apphttp "recallweave/internal/http"
	"recallweave/internal/ingest"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/retrieval"
	"recallweave/internal/run"
	"recallweave/internal/session"
	"recallweave/internal/types"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func workflowPostgres(t *testing.T) (context.Context, *gorm.DB, string) {
	t.Helper()
	return workflowPostgresWithTimeout(t, 60*time.Second)
}

func workflowPostgresWithTimeout(t *testing.T, timeout time.Duration) (context.Context, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("RECALLWEAVE_WORKFLOW_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RECALLWEAVE_WORKFLOW_DATABASE_URL to run HTTP/PostgreSQL workflow acceptance")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") ||
		endpoint.Path != "/recallweave_workflow_test" ||
		(endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost") {
		t.Fatal("workflow acceptance requires an explicit local recallweave_workflow_test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	t.Cleanup(func() {
		cancel()
		cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if schema != "" {
			if err := admin.WithContext(cleanupCtx).Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error; err != nil {
				t.Errorf("clean up workflow schema: %v", err)
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
	schema = fmt.Sprintf("recallweave_workflow_%x", nonce)
	if err := admin.WithContext(ctx).Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	query := endpoint.Query()
	query.Set("search_path", schema+",public")
	query.Set("application_name", schema)
	endpoint.RawQuery = query.Encode()
	scopedDSN := endpoint.String()
	database := workflowOpenDatabase(t, scopedDSN)
	if err := appdb.AutoMigrate(database.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	return ctx, database, scopedDSN
}

func workflowOpenDatabase(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	connection.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = connection.Close() })
	return database
}

func workflowServer(t *testing.T, database *gorm.DB, model *llm.Client) (*httptest.Server, *extract.ExtractExcuter) {
	t.Helper()
	memories := memory.NewMemoryManger(database, retrieval.New(database, model))
	sessions := session.NewSessionManager(database)
	extractor := extract.NewExtractExcuter(database, model, model)
	conversations := conversation.NewConversationManager(database)
	runs := run.NewRunManager(database)
	core := agent.NewAgent(1, "workflow", model, "RecallWeave 测试助手", 6, ability.NewMemoryAbility(memories, sessions))
	service := chat.NewChatService(core, memories, sessions, extractor).
		WithConversationManager(conversations).
		WithRunManager(runs).
		WithTurnWriter(chat.NewReportStore(database))
	router := apphttp.NewRouter(
		apphttp.NewIngestHandler(ingest.NewService(database, extractor)),
		apphttp.NewExtractHandler(extractor),
		apphttp.NewChatHandler(service),
		apphttp.NewConversationHandler(conversations, sessions),
		apphttp.NewSessionHandler(sessions, memories),
		apphttp.NewRunHandler(runs),
	)
	apphttp.MountConversationEvents(router, apphttp.NewEventsHandler(events.NewStore(database)))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, extractor
}

func workflowJSON(t *testing.T, ctx context.Context, server *httptest.Server, method, path string, body any, wantStatus int, target any) []byte {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, response.StatusCode, wantStatus, raw)
	}
	if target != nil {
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, raw)
		}
	}
	return raw
}

type workflowHistory struct {
	SessionID uint            `json:"session_id"`
	Messages  []types.Message `json:"messages"`
}

type workflowOverview struct {
	ID            uint   `json:"id"`
	ExtractStatus string `json:"extract_status"`
	ExtractError  string `json:"extract_error"`
	MessageCount  int    `json:"message_count"`
	MemoryCount   int    `json:"memory_count"`
}

func workflowWaitExtraction(t *testing.T, ctx context.Context, database *gorm.DB, server *httptest.Server, sessionID uint, lastSeq, memoryCount int) workflowOverview {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var overview workflowOverview
		workflowJSON(t, ctx, server, http.MethodGet, fmt.Sprintf("/api/sessions/%d", sessionID), nil, http.StatusOK, &overview)
		if overview.ExtractStatus == types.ExtractFailed {
			t.Fatalf("background extraction failed: %+v", overview)
		}
		var coverage struct{ Seq int }
		if err := database.WithContext(ctx).Model(&types.Memory{}).Where("session_id = ?", sessionID).
			Select("COALESCE(MAX(seq_end), 0) AS seq").Scan(&coverage).Error; err != nil {
			t.Fatal(err)
		}
		if overview.ExtractStatus == types.ExtractDone && overview.MessageCount == lastSeq &&
			overview.MemoryCount == memoryCount && coverage.Seq == lastSeq {
			return overview
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("extraction did not converge: %+v coverage=%d", overview, coverage.Seq)
		}
	}
}

func TestPostgresHTTPChatMemoryWorkflow(t *testing.T) {
	ctx, database, dsn := workflowPostgres(t)
	fake, modelServer := newWorkflowModel(t)
	client, err := llm.NewClient(llm.Config{
		BaseURL: modelServer.URL + "/v1", APIKey: "workflow-test-only",
		Model: "workflow-chat", EmbeddingModel: "workflow-embedding", Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, extractor := workflowServer(t, database, client)
	var source types.Conversation
	workflowJSON(t, ctx, server, http.MethodPost, "/api/conversations/create", map[string]any{"title": ""}, http.StatusCreated, &source)
	if source.ID == 0 {
		t.Fatal("conversation has no persisted identity")
	}
	sourcePath := fmt.Sprintf("/api/conversations/%d", source.ID)
	if raw := workflowJSON(t, ctx, server, http.MethodGet, sourcePath+"/runs", nil, http.StatusOK, nil); string(raw) != `{"runs":[]}` {
		t.Fatalf("new conversation does not have an empty runs array: %s", raw)
	}
	var empty workflowHistory
	workflowJSON(t, ctx, server, http.MethodGet, sourcePath+"/messages", nil, http.StatusOK, &empty)
	if len(empty.Messages) != 0 {
		t.Fatal("new conversation has unexpected messages")
	}

	var first, second chat.Answer
	workflowJSON(t, ctx, server, http.MethodPost, fmt.Sprintf("/api/chat/%d", source.ID), map[string]string{"question": workflowFirstQuestion}, http.StatusOK, &first)
	if first.Text != workflowFirstAnswer || first.RunID == "" {
		t.Fatalf("first chat result: %+v", first)
	}
	if err := fake.WaitExtractionStarted(ctx); err != nil {
		t.Fatal(err)
	}
	workflowJSON(t, ctx, server, http.MethodPost, fmt.Sprintf("/api/chat/%d", source.ID), map[string]string{"question": workflowSecondQuestion}, http.StatusOK, &second)
	if second.Text != workflowSecondAnswer || second.RunID == "" || second.RunID == first.RunID {
		t.Fatalf("second chat result: %+v", second)
	}
	fake.AssertHistory(t)
	fake.AssertOrdinaryChatWithoutMemoryReads(t)
	var history workflowHistory
	workflowJSON(t, ctx, server, http.MethodGet, sourcePath+"/messages", nil, http.StatusOK, &history)
	wantContents := []string{workflowFirstQuestion, workflowFirstAnswer, workflowSecondQuestion, workflowSecondAnswer}
	if history.SessionID == 0 || len(history.Messages) != len(wantContents) {
		t.Fatalf("chat history incomplete: %+v", history)
	}
	for i, message := range history.Messages {
		wantRole := "user"
		if i%2 == 1 {
			wantRole = "assistant"
		}
		if message.ID == 0 || message.SessionID != history.SessionID || message.Seq != i+1 || message.Role != wantRole || message.Content != wantContents[i] {
			t.Fatalf("history identity/order/content mismatch at %d: %+v", i, message)
		}
	}
	var count int64
	if err := database.WithContext(ctx).Model(&types.Memory{}).Where("session_id = ?", history.SessionID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("first extraction did not remain blocked while chat continued: count=%d err=%v", count, err)
	}
	fake.ReleaseExtraction()
	overview := workflowWaitExtraction(t, ctx, database, server, history.SessionID, 4, 2)
	var memoryResponse struct {
		Memories []types.Memory `json:"memories"`
	}
	workflowJSON(t, ctx, server, http.MethodGet, fmt.Sprintf("/api/sessions/%d/memories", history.SessionID), nil, http.StatusOK, &memoryResponse)
	if len(memoryResponse.Memories) != 2 {
		t.Fatalf("memory listing did not return the two extracted turns: %+v", memoryResponse.Memories)
	}
	for i, item := range memoryResponse.Memories {
		if item.ID == 0 || item.SessionID != history.SessionID || item.SeqStart != 2*i+1 || item.SeqEnd != 2*i+2 {
			t.Fatalf("duplicate or missing extraction range: %+v", memoryResponse.Memories)
		}
	}
	if added, err := extractor.ExtractIncrementalToMemory(ctx, history.SessionID); err != nil || added != 0 {
		t.Fatalf("repeat extraction was not idempotent: added=%d err=%v", added, err)
	}
	fake.AssertOrdinaryChatWithoutMemoryReads(t)
	ordinaryStats := fake.Stats()
	if ordinaryStats.MemoryEmbeddingCalls != 2 || ordinaryStats.EmbeddingCalls != ordinaryStats.MemoryEmbeddingCalls {
		t.Fatalf("ordinary chat embeddings were not limited to the two saved memories: %+v inputs=%q", ordinaryStats, fake.EmbeddingInputs())
	}

	// A new DB pool and newly assembled HTTP router must read saved state.
	reloadedDB := workflowOpenDatabase(t, dsn)
	reloaded, _ := workflowServer(t, reloadedDB, client)
	var restored workflowHistory
	workflowJSON(t, ctx, reloaded, http.MethodGet, sourcePath+"/messages", nil, http.StatusOK, &restored)
	if !reflect.DeepEqual(restored, history) {
		t.Fatalf("history changed after reopening storage: got %+v want %+v", restored, history)
	}
	var savedRuns struct {
		Runs []types.AgentRun `json:"runs"`
	}
	workflowJSON(t, ctx, reloaded, http.MethodGet, sourcePath+"/runs", nil, http.StatusOK, &savedRuns)
	if len(savedRuns.Runs) != 2 || savedRuns.Runs[0].RunID != second.RunID || savedRuns.Runs[1].RunID != first.RunID {
		t.Fatalf("saved Run linkage/order incorrect: %+v", savedRuns.Runs)
	}
	for _, saved := range savedRuns.Runs {
		if saved.Status != string(runtime.RunCompleted) || saved.ConversationID != source.ID {
			t.Fatalf("unexpected persisted Run: %+v", saved)
		}
		var state runtime.RunState
		if err := json.Unmarshal(saved.State, &state); err != nil {
			t.Fatal(err)
		}
		if len(state.Steps) != 0 || state.CurrentRound != 1 {
			t.Fatalf("ordinary chat persisted an unexpected tool round: %+v", state)
		}
	}
	storedEvents, err := events.NewStore(reloadedDB).List(ctx, source.ID, 0, 20)
	if err != nil || len(storedEvents) != 4 {
		t.Fatalf("saved message events: count=%d err=%v", len(storedEvents), err)
	}
	for i, event := range storedEvents {
		if event.Kind != "message.created" || event.Key != fmt.Sprintf("message:%d", history.Messages[i].ID) || (i > 0 && event.ID <= storedEvents[i-1].ID) {
			t.Fatalf("message event linkage/order changed: %+v", storedEvents)
		}
	}

	var recallConversation types.Conversation
	workflowJSON(t, ctx, reloaded, http.MethodPost, "/api/conversations/create", map[string]string{"title": "历史核对"}, http.StatusCreated, &recallConversation)
	var recalled chat.Answer
	workflowJSON(t, ctx, reloaded, http.MethodPost, fmt.Sprintf("/api/chat/%d", recallConversation.ID), map[string]string{"question": workflowRecallQuestion}, http.StatusOK, &recalled)
	if recalled.Text != workflowRecallAnswer || recalled.RunID == "" || len(recalled.Citations) != 1 {
		t.Fatalf("historical answer did not contain a verified citation: %+v", recalled)
	}
	citation := recalled.Citations[0]
	if citation.MemoryID != memoryResponse.Memories[0].ID || citation.SessionID != history.SessionID || citation.SeqStart != 1 || citation.SeqEnd != 2 {
		t.Fatalf("citation did not point to original first turn: %+v", citation)
	}
	var citedHistory workflowHistory
	workflowJSON(t, ctx, reloaded, http.MethodGet, fmt.Sprintf("/api/sessions/%d/messages", citation.SessionID), nil, http.StatusOK, &citedHistory)
	if len(citedHistory.Messages) != 4 || citedHistory.Messages[0].Content != workflowFirstQuestion {
		t.Fatalf("citation source cannot be read: %+v", citedHistory)
	}
	var savedRecall types.AgentRun
	workflowJSON(t, ctx, reloaded, http.MethodGet, "/api/runs/"+recalled.RunID, nil, http.StatusOK, &savedRecall)
	var state runtime.RunState
	if err := json.Unmarshal(savedRecall.State, &state); err != nil {
		t.Fatal(err)
	}
	if savedRecall.Input != workflowRecallQuestion || savedRecall.Content != workflowRecallAnswer || state.Status != runtime.RunCompleted || state.CurrentRound != recalled.Rounds || len(state.Steps) != 2 {
		t.Fatalf("recall Run trace mismatch: %+v", state)
	}
	for i, tool := range []string{ability.SearchMemoriesToolName, ability.GetMessagesToolName} {
		if state.Steps[i].ToolName != tool || state.Steps[i].Error != "" || state.Steps[i].Result == "" {
			t.Fatalf("recall did not execute and persist tool %s: %+v", tool, state.Steps)
		}
	}
	if sources := runtime.SourcesForRun(state); len(sources) != 1 || sources[0].SessionID != history.SessionID || sources[0].SeqStart != 1 || sources[0].SeqEnd != 2 {
		t.Fatalf("Run sources do not match read original messages: %+v", sources)
	}
	var recallHistory workflowHistory
	workflowJSON(t, ctx, reloaded, http.MethodGet, fmt.Sprintf("/api/conversations/%d/messages", recallConversation.ID), nil, http.StatusOK, &recallHistory)
	workflowWaitExtraction(t, ctx, reloadedDB, reloaded, recallHistory.SessionID, 2, 1)
	workflowJSON(t, ctx, reloaded, http.MethodGet, sourcePath+"/runs", nil, http.StatusOK, &savedRuns)
	if len(savedRuns.Runs) != 2 {
		t.Fatalf("other conversation contaminated source Run list: %+v", savedRuns.Runs)
	}
	fake.AssertHistory(t)
	finalStats := fake.Stats()
	if finalStats.SearchCalls != 1 || finalStats.SourceCalls != 1 || finalStats.QueryEmbeddingCalls != 1 ||
		finalStats.MemoryEmbeddingCalls != 3 || finalStats.EmbeddingCalls != finalStats.QueryEmbeddingCalls+finalStats.MemoryEmbeddingCalls {
		t.Fatalf("historical recall did not use exactly one on-demand search plus source read: %+v inputs=%q", finalStats, fake.EmbeddingInputs())
	}
	t.Logf("PASS source conversation=%d messages=4 runs=2 memories=2 ranges=1-2,3-4; recall conversation=%d rounds=%d citation=session:%d seq:1-2; model=%+v",
		source.ID, recallConversation.ID, recalled.Rounds, citation.SessionID, finalStats)
	if reportPath := os.Getenv("RECALLWEAVE_WORKFLOW_REPORT_PATH"); reportPath != "" {
		report := map[string]any{
			"validation":          "real HTTP, production services and PostgreSQL; deterministic local model responses",
			"checks":              []string{"new conversation empty runs", "two ordered chat turns", "ordinary chat performs no memory tools or query embeddings", "background extraction still embeds two memories", "chat continues during blocked extraction", "two nonoverlapping memory ranges", "repeat extraction adds zero memories", "reopened storage preserves history and runs", "four message events persisted", "cross-conversation on-demand search and original-message verification", "citation resolves to source messages", "Run records persist tool trace"},
			"source_conversation": source, "source_history": history, "source_overview": overview,
			"source_memories": memoryResponse.Memories, "source_runs": savedRuns.Runs,
			"recall_answer": recalled, "recall_run": savedRecall, "model_calls": finalStats,
			"ordinary_chat_model_calls": ordinaryStats, "embedding_inputs": fake.EmbeddingInputs(),
		}
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reportPath, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("workflow report: %s", reportPath)
	}
}
