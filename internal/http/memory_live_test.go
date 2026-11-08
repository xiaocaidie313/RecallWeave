//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/chat"
	"recallweave/internal/config"
	"recallweave/internal/conversation"
	apphttp "recallweave/internal/http"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/retrieval"
	"recallweave/internal/run"
	"recallweave/internal/session"
	"recallweave/internal/types"
)

type liveMemoryCase struct {
	Name           string          `json:"name"`
	Question       string          `json:"question"`
	ConversationID uint            `json:"conversation_id"`
	HTTPStatus     int             `json:"http_status"`
	Answer         chat.Answer     `json:"answer"`
	Run            types.AgentRun  `json:"run"`
	Checks         map[string]bool `json:"checks"`
	Error          string          `json:"error,omitempty"`
	ElapsedMs      int64           `json:"elapsed_ms"`
	Passed         bool            `json:"passed"`
}

// This test makes real paid calls only after an explicit opt-in. It evaluates
// production memory routing, not extraction, external tools, or delegation.
func TestLiveMemoryAcceptance(t *testing.T) {
	if os.Getenv("RECALLWEAVE_LIVE_MODEL") != "1" {
		t.Skip("set RECALLWEAVE_LIVE_MODEL=1 to allow real configured-model calls")
	}
	configPath := os.Getenv("RECALLWEAVE_LIVE_CONFIG_PATH")
	if configPath == "" {
		configPath = filepath.Join("..", "..", "config", "config.yaml")
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal("load live model configuration: ", err)
	}
	if cfg.LLM.APIKey == "" {
		t.Fatal("live model API key is not configured")
	}
	redact := func(value string) string { return strings.ReplaceAll(value, cfg.LLM.APIKey, "[redacted]") }
	client, err := llm.NewClient(llm.Config{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey,
		Model: cfg.LLM.Model, EmbeddingModel: cfg.LLM.EmbeddingModel, Timeout: time.Duration(cfg.LLM.TimeoutSeconds) * time.Second})
	if err != nil {
		t.Fatal("initialize live model client: ", redact(err.Error()))
	}
	ctx, database, _ := workflowPostgresWithTimeout(t, 6*time.Minute)
	sessions := session.NewSessionManager(database)
	conversations := conversation.NewConversationManager(database)
	runs := run.NewRunManager(database)
	memories := memory.NewMemoryManger(database, retrieval.New(database, client))
	writer := chat.NewReportStore(database)
	core := agent.NewAgent(1, "live-memory", client, "RecallWeave 记忆助手", 6, ability.NewMemoryAbility(memories, sessions))
	service := chat.NewChatService(core, memories, sessions, nil).
		WithConversationManager(conversations).WithRunManager(runs).WithTurnWriter(writer)
	server := httptest.NewServer(apphttp.NewRouter(nil, nil, apphttp.NewChatHandler(service),
		apphttp.NewConversationHandler(conversations, sessions), apphttp.NewSessionHandler(sessions, memories), apphttp.NewRunHandler(runs)))
	defer server.Close()

	const sourceQuestion = "RecallWeave 项目选择 PostgreSQL，因为需要关系查询和 pgvector 向量检索，希望聊天记录和记忆保存在同一个数据库。"
	const sourceAnswer = "已确认 RecallWeave 的 PostgreSQL 选型理由：关系查询、pgvector，以及统一保存聊天记录和记忆。"
	source, err := conversations.CreateConversation(ctx, "真实模型验收：PostgreSQL 原始记录")
	if err != nil {
		t.Fatal(err)
	}
	sourceSession, err := sessions.ChatSession(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AppendTurn(ctx, sourceSession.ID, sourceQuestion, sourceAnswer); err != nil {
		t.Fatal(err)
	}
	seed := types.Memory{SessionID: sourceSession.ID, SeqStart: 1, SeqEnd: 2, Title: "RecallWeave PostgreSQL 选型理由",
		Content: sourceQuestion, Tag: types.TagTech, ContentType: types.ContentDecision, Status: types.MemoryActive}
	embeddingStarted := time.Now()
	vector, embeddingErr := client.Embed(ctx, seed.Title+"\n"+seed.Content)
	embeddingElapsedMs := time.Since(embeddingStarted).Milliseconds()
	seed.Embedding = types.NewEmbedding(vector)
	storedDimensions := len(types.EmbeddingValues(seed.Embedding))
	vectorCompatible := embeddingErr == nil && storedDimensions == types.EmbeddingDim
	embeddingError := ""
	if embeddingErr != nil {
		embeddingError = redact(embeddingErr.Error())
	} else if !vectorCompatible {
		embeddingError = fmt.Sprintf("unsupported embedding dimensions: raw=%d stored=%d expected stored=%d", len(vector), storedDimensions, types.EmbeddingDim)
	}
	if vectorCompatible {
		err = database.WithContext(ctx).Create(&seed).Error
	} else {
		// An explicit degraded fixture lets all four behavior cases run. This
		// cannot pass the complete acceptance: vectorCompatible remains false.
		err = database.WithContext(ctx).Omit("Embedding").Create(&seed).Error
	}
	if err != nil {
		t.Fatal("seed source memory: ", err)
	}
	vectorQuery := "RecallWeave PostgreSQL 关系查询 pgvector"
	vectorHits, vectorErr := retrieval.New(database, client).Search(ctx, vectorQuery, types.TagTech, 5)
	vectorIDs := make([]uint, 0, len(vectorHits))
	vectorSeedMatched := false
	for _, hit := range vectorHits {
		vectorIDs = append(vectorIDs, hit.Memory.ID)
		vectorSeedMatched = vectorSeedMatched || hit.Memory.ID == seed.ID
	}
	vectorProbeError := ""
	if vectorErr != nil {
		vectorProbeError = redact(vectorErr.Error())
	}

	cases := []liveMemoryCase{
		{Name: "ordinary_go_question", Question: "Go 语言里接口和结构体有什么区别？请简短说明。"},
		{Name: "current_history_sufficient", Question: "当前这个会话里，我们已经决定用哪种缓存，如何部署？"},
		{Name: "cross_conversation_postgresql", Question: "在另一个会话里，RecallWeave 项目之前为什么选择 PostgreSQL？请核对原始记录后回答并引用依据。"},
		{Name: "missing_cloud_provider", Question: "我之前决定使用哪家云厂商？只按已保存记录回答，不要推荐。如果没有明确记录，请回答：记录中未说明云厂商。"},
	}
	for i := range cases {
		result := &cases[i]
		result.Checks = make(map[string]bool)
		started := time.Now()
		conv, setupErr := conversations.CreateConversation(ctx, "真实模型验收："+result.Name)
		if setupErr != nil {
			result.Error = redact(setupErr.Error())
			continue
		}
		result.ConversationID = conv.ID
		if result.Name == "current_history_sufficient" {
			current, currentErr := sessions.ChatSession(ctx, conv.ID)
			if currentErr == nil {
				currentErr = writer.AppendTurn(ctx, current.ID, "这次缓存部署选 Valkey，通过 Docker Compose 在本机启动。", "好的，本次缓存选择 Valkey，先通过 Docker Compose 在本机部署。")
			}
			if currentErr != nil {
				result.Error = redact(currentErr.Error())
				continue
			}
		}
		caseCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		body, _ := json.Marshal(map[string]string{"question": result.Question})
		request, requestErr := http.NewRequestWithContext(caseCtx, http.MethodPost, fmt.Sprintf("%s/api/chat/%d", server.URL, conv.ID), bytes.NewReader(body))
		if requestErr == nil {
			request.Header.Set("Content-Type", "application/json")
			var response *http.Response
			response, requestErr = server.Client().Do(request)
			if requestErr == nil {
				result.HTTPStatus = response.StatusCode
				var raw []byte
				raw, requestErr = io.ReadAll(response.Body)
				_ = response.Body.Close()
				if requestErr == nil && response.StatusCode == http.StatusOK {
					requestErr = json.Unmarshal(raw, &result.Answer)
				} else if requestErr == nil {
					requestErr = fmt.Errorf("HTTP %d: %s", response.StatusCode, raw)
				}
			}
		}
		cancel()
		result.ElapsedMs = time.Since(started).Milliseconds()
		if requestErr != nil {
			result.Error = redact(requestErr.Error())
		}
		// Failed runs are still useful evidence even when the HTTP error did
		// not expose run_id. Query only this synthetic conversation.
		if err := database.WithContext(ctx).Where("conversation_id = ?", conv.ID).Order("id DESC").First(&result.Run).Error; err != nil {
			result.Error += "; load saved run: " + redact(err.Error())
		}
		var state runtime.RunState
		if err := json.Unmarshal(result.Run.State, &state); err != nil {
			result.Error += "; decode saved run: " + err.Error()
		}
		steps := state.AllSteps()
		result.Checks["completed"] = result.HTTPStatus == http.StatusOK && state.Status == runtime.RunCompleted && strings.TrimSpace(result.Answer.Text) != ""
		switch result.Name {
		case "ordinary_go_question", "current_history_sufficient":
			result.Checks["no_memory_tools"] = !liveHasMemoryTools(steps)
			result.Checks["no_memory_citations"] = len(result.Answer.Citations) == 0
			if result.Name == "current_history_sufficient" {
				lower := strings.ToLower(result.Answer.Text)
				result.Checks["current_history_fact"] = strings.Contains(lower, "valkey") && strings.Contains(lower, "docker compose")
			}
		case "cross_conversation_postgresql":
			lower := strings.ToLower(result.Answer.Text)
			result.Checks["postgresql_fixture_reasons"] = strings.Contains(lower, "postgresql") && strings.Contains(lower, "关系") &&
				(strings.Contains(lower, "pgvector") || strings.Contains(lower, "向量"))
			result.Checks["search_then_original_source"] = liveVerifiedSource(steps, seed, sourceQuestion)
			result.Checks["citation_points_to_seed"] = false
			for _, citation := range result.Answer.Citations {
				if citation.MemoryID == seed.ID && citation.SessionID == seed.SessionID && citation.SeqStart == 1 && citation.SeqEnd >= 1 && citation.SeqEnd <= 2 {
					result.Checks["citation_points_to_seed"] = true
				}
			}
		case "missing_cloud_provider":
			result.Checks["exact_missing_record_reply"] = strings.TrimSpace(result.Answer.Text) == "记录中未说明云厂商。"
			result.Checks["successful_history_search"] = false
			for _, step := range steps {
				if step.ToolName == ability.SearchMemoriesToolName && step.Error == "" {
					var search ability.SearchMemoriesResult
					if json.Unmarshal([]byte(step.Result), &search) == nil {
						result.Checks["successful_history_search"] = true
					}
				}
			}
		}
		result.Passed = result.Error == ""
		for _, passed := range result.Checks {
			result.Passed = result.Passed && passed
		}
		t.Logf("live case=%s HTTP=%d passed=%v rounds=%d elapsed_ms=%d", result.Name, result.HTTPStatus, result.Passed, result.Answer.Rounds, result.ElapsedMs)
	}

	reportPath := os.Getenv("RECALLWEAVE_LIVE_REPORT_PATH")
	if reportPath == "" {
		reportPath = filepath.Join("..", "..", ".gocache", "memory-live-acceptance.json")
	}
	report := map[string]any{
		"validation": "real configured model and embedding API, production HTTP/SDK/services, isolated real PostgreSQL",
		"model":      cfg.LLM.Model, "embedding_model": cfg.LLM.EmbeddingModel,
		"seed": map[string]any{"memory_id": seed.ID, "session_id": seed.SessionID, "source_question": sourceQuestion, "source_answer": sourceAnswer,
			"raw_embedding_dimensions": len(vector), "stored_embedding_dimensions": storedDimensions, "vector_compatible": vectorCompatible,
			"embedding_error": embeddingError, "embedding_elapsed_ms": embeddingElapsedMs},
		"vector_only_probe": map[string]any{"query": vectorQuery, "hit_memory_ids": vectorIDs, "seed_matched": vectorSeedMatched, "error": vectorProbeError},
		"limitations": []string{"four fixed scenarios, one attempt each; no benchmarked accuracy", "source messages/memory and current Valkey history seeded directly; extraction not evaluated",
			"no extractor, web, MCP, delegator, artifacts or skills; built-in runtime planning remains available", "nondefault titles avoid title model calls; production SDK retry behavior unchanged"},
		"cases": cases,
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte(redact(string(encoded))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("live model report: %s", reportPath)
	if !vectorCompatible {
		t.Error("live embedding compatibility failed: ", embeddingError)
	}
	if vectorErr != nil || !vectorSeedMatched {
		t.Errorf("production vector-only retrieval did not match the seeded memory: matched=%v error=%s", vectorSeedMatched, vectorProbeError)
	}
	for _, result := range cases {
		if !result.Passed {
			t.Errorf("live memory case %s failed: checks=%v error=%s", result.Name, result.Checks, result.Error)
		}
	}
}

func liveHasMemoryTools(steps []runtime.RunStep) bool {
	for _, step := range steps {
		switch step.ToolName {
		case ability.SearchMemoriesToolName, ability.GetMessagesToolName, "get_sessions", ability.DeactivateMemoryToolName:
			return true
		}
	}
	return false
}

func liveVerifiedSource(steps []runtime.RunStep, seed types.Memory, source string) bool {
	searched := false
	for _, step := range steps {
		if step.Error != "" {
			continue
		}
		if step.ToolName == ability.SearchMemoriesToolName {
			var result ability.SearchMemoriesResult
			if json.Unmarshal([]byte(step.Result), &result) == nil {
				for _, hit := range result.Memories {
					searched = searched || hit.MemoryID == seed.ID && hit.SessionID == seed.SessionID
				}
			}
		}
		if searched && step.ToolName == ability.GetMessagesToolName {
			var result struct {
				SessionID uint            `json:"session_id"`
				Messages  []types.Message `json:"messages"`
			}
			if json.Unmarshal([]byte(step.Result), &result) == nil && result.SessionID == seed.SessionID {
				for _, message := range result.Messages {
					if message.Seq == 1 && message.Content == source {
						return true
					}
				}
			}
		}
	}
	return false
}
