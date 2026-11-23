//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/chat"
	"recallweave/internal/conversation"
	"recallweave/internal/extract"
	apphttp "recallweave/internal/http"
	"recallweave/internal/ingest"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/run"
	"recallweave/internal/session"
	"recallweave/internal/skill"
	"recallweave/internal/thinking"
	"recallweave/internal/types"

	"gorm.io/gorm"
)

const (
	writingNote      = "写给朋友时，我写完才看清自己的意思。"
	oldAssistantNote = "你可能只在调试解释器时才会集中精神。"
	musicNote        = "练琴时，给别人解释指法才发现自己没懂。"
	openQuestion     = "我写给自己时也有这种感觉，这条线能走多远？"
	firstThought     = "我为什么总是在写完之后才想明白？"
	resumePrompt     = "接着我留下的思考继续。"
	firstHypothesis  = "我猜是解释让你听见了想法中的空白。听众必须在场吗？"
	secondHypothesis = "我们可以把解释和收到回应这两个条件拆开想。"
)

type thinkingScenario struct{}

type thinkingModelMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	CallID  string `json:"tool_call_id"`
}

// The model is a fixture. It checks the data actually sent by the production
// runtime and requests the next tool using IDs received from the real search.
func (thinkingScenario) Next(_ context.Context, conv *llm.Conversation, _ []llm.ToolSchema) (llm.Turn, error) {
	snapshot, err := conv.Snapshot()
	if err != nil {
		return llm.Turn{}, err
	}
	var messages []thinkingModelMessage
	if err := json.Unmarshal(snapshot.Messages, &messages); err != nil {
		return llm.Turn{}, err
	}
	turn, err := nextThinkingStep(messages)
	if err == nil {
		conv.AddAssistantTurn(turn)
	}
	return turn, err
}

func (thinkingScenario) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("unexpected auxiliary model call")
}

func nextThinkingStep(messages []thinkingModelMessage) (llm.Turn, error) {
	last := messages[len(messages)-1]
	if last.Role == "user" && len(messages) >= 3 {
		reference := messages[len(messages)-2]
		start := strings.IndexByte(reference.Content, '{')
		if reference.Role != "user" || start < 0 {
			return llm.Turn{}, fmt.Errorf("missing user-level thinking context")
		}
		var data struct {
			Originals []types.ContextMessage    `json:"selected_originals"`
			Point     *types.ThinkingCheckpoint `json:"user_checkpoint"`
		}
		if err := json.Unmarshal([]byte(reference.Content[start:]), &data); err != nil {
			return llm.Turn{}, err
		}
		switch last.Content {
		case "读回选中的第二段":
			if len(data.Originals) != 1 || data.Originals[0].Content != "第二段。" || data.Originals[0].Seq != 2 {
				return llm.Turn{}, fmt.Errorf("selected local note passage was lost: %+v", data)
			}
			return llm.Turn{Content: "第二段。"}, nil
		case firstThought:
			if len(data.Originals) != 2 || data.Originals[0].Role != "user" || data.Originals[0].Content != writingNote ||
				data.Originals[1].Role != "assistant" || data.Originals[1].Content != oldAssistantNote {
				return llm.Turn{}, fmt.Errorf("selected note lost original speakers or text: %+v", data)
			}
			return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "search", Name: ability.SearchMemoriesToolName, Arguments: `{"keyword":"解释"}`}}}, nil
		case resumePrompt:
			if data.Point == nil || data.Point.Author != "user" || data.Point.Direction != "reframe" || data.Point.Text != openQuestion || len(data.Originals) != 0 {
				return llm.Turn{}, fmt.Errorf("resumed input lost the user's checkpoint: %+v", data)
			}
			return llm.Turn{Content: secondHypothesis}, nil
		}
	}
	switch last.CallID {
	case "search":
		var hits ability.SearchMemoriesResult
		if err := json.Unmarshal([]byte(last.Content), &hits); err != nil {
			return llm.Turn{}, err
		}
		var musicSession uint
		var sawDistractor bool
		for _, hit := range hits.Memories {
			if strings.Contains(hit.Content, musicNote) {
				musicSession = hit.SessionID
			}
			sawDistractor = sawDistractor || strings.Contains(hit.Content, oldAssistantNote)
		}
		if musicSession == 0 || !sawDistractor {
			return llm.Turn{}, fmt.Errorf("missing source or keyword distractor: %+v", hits)
		}
		args := fmt.Sprintf(`{"session_id":%d,"seq_start":1,"seq_end":1}`, musicSession)
		return llm.Turn{ToolCalls: []llm.ToolCall{{ID: "original", Name: ability.GetMessagesToolName, Arguments: args}}}, nil
	case "original":
		var source struct{ Messages []types.Message }
		if err := json.Unmarshal([]byte(last.Content), &source); err != nil {
			return llm.Turn{}, err
		}
		if len(source.Messages) != 1 || source.Messages[0].Role != "user" || source.Messages[0].Content != musicNote {
			return llm.Turn{}, fmt.Errorf("wrong original evidence: %+v", source)
		}
		return llm.Turn{Content: firstHypothesis}, nil
	}
	return llm.Turn{}, fmt.Errorf("unexpected thinking step: %+v", last)
}

func thinkingWorkflowServer(t *testing.T, db *gorm.DB) *httptest.Server {
	t.Helper()
	catalog, err := skill.Open(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	memories := memory.NewMemoryManger(db, nil)
	sessions := session.NewSessionManager(db)
	conversations := conversation.NewConversationManager(db)
	extractor := extract.NewExtractExcuter(db, nil, nil)
	runs := run.NewRunManager(db)
	points := thinking.NewStore(db)
	core := agent.NewAgent(1, "thinking-test", thinkingScenario{}, "", 5, ability.NewMemoryAbility(memories, sessions)).WithSkills(catalog)
	service := chat.NewChatService(core, memories, sessions, extractor).WithConversationManager(conversations).
		WithRunManager(runs).WithTurnWriter(chat.NewReportStore(db)).WithThinkingStore(points)
	router := apphttp.NewRouter(apphttp.NewIngestHandler(ingest.NewService(db, extractor)), apphttp.NewExtractHandler(extractor),
		apphttp.NewChatHandler(service), apphttp.NewConversationHandler(conversations, sessions), apphttp.NewSessionHandler(sessions, memories), apphttp.NewRunHandler(runs))
	apphttp.MountThinkingCheckpoints(router, points)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func TestPostgresThinkingPartnerWorkflow(t *testing.T) {
	ctx, db, dsn := workflowPostgres(t)
	server := thinkingWorkflowServer(t, db)
	importNote := func(user, assistant string) uint {
		t.Helper()
		var imported struct {
			Sessions []struct {
				SessionID uint `json:"session_id"`
			}
		}
		workflowJSON(t, ctx, server, http.MethodPost, "/api/imports", map[string]string{
			"source_tag": "codex", "format": "markdown", "text": "## User\n" + user + "\n## Assistant\n" + assistant,
		}, http.StatusCreated, &imported)
		if len(imported.Sessions) != 1 || imported.Sessions[0].SessionID == 0 {
			t.Fatalf("missing imported note: %+v", imported)
		}
		id := imported.Sessions[0].SessionID
		workflowWaitExtraction(t, ctx, db, server, id, 2, 1)
		return id
	}
	writing := importNote(writingNote, oldAssistantNote)
	music := importNote(musicNote, "我听明白了。")
	startConversation := func(s *httptest.Server) uint {
		t.Helper()
		var conv types.Conversation
		workflowJSON(t, ctx, s, http.MethodPost, "/api/conversations/create", map[string]string{"title": "思考验收"}, http.StatusCreated, &conv)
		return conv.ID
	}
	first := startConversation(server)
	anchor := types.ThoughtSource{SessionID: writing, SeqStart: 1, SeqEnd: 2}
	var answer chat.Answer
	workflowJSON(t, ctx, server, http.MethodPost, fmt.Sprintf("/api/chat/%d", first), map[string]any{
		"mode": "thinking", "question": firstThought, "source": anchor,
	}, http.StatusOK, &answer)
	if answer.Text != firstHypothesis || answer.Thinking == nil || answer.Thinking.Navigation || !reflect.DeepEqual(answer.Thinking.Source, &anchor) || len(answer.Citations) != 1 ||
		answer.Citations[0].SessionID != music || answer.Citations[0].SeqStart != 1 || answer.Citations[0].SeqEnd != 1 {
		t.Fatalf("wrong answer or verified source: %+v", answer)
	}
	var point struct{ Checkpoint types.ThinkingCheckpoint }
	workflowJSON(t, ctx, server, http.MethodPost, fmt.Sprintf("/api/conversations/%d/thinking/checkpoint", first),
		map[string]string{"text": openQuestion, "direction": "reframe"}, http.StatusOK, &point)
	assertThinkingTranscript(t, ctx, db, server, first, firstThought, answer)
	server.Close()

	reopened := workflowOpenDatabase(t, dsn)
	restarted := thinkingWorkflowServer(t, reopened)
	second := startConversation(restarted)
	var resumed chat.Answer
	workflowJSON(t, ctx, restarted, http.MethodPost, fmt.Sprintf("/api/chat/%d", second), map[string]any{
		"mode": "thinking", "resume_from": first,
	}, http.StatusOK, &resumed)
	if resumed.Text != secondHypothesis || resumed.Thinking == nil || !resumed.Thinking.Navigation || resumed.Thinking.Source != nil ||
		!reflect.DeepEqual(resumed.Thinking.Checkpoint, &point.Checkpoint) || point.Checkpoint.ConversationID != first || len(resumed.Citations) != 0 {
		t.Fatalf("resume lost checkpoint provenance or invented citations: %+v", resumed)
	}
	assertThinkingTranscript(t, ctx, reopened, restarted, second, resumePrompt, resumed)
	// A later edit must not change the historical starting point of a Run.
	workflowJSON(t, ctx, restarted, http.MethodPost, fmt.Sprintf("/api/conversations/%d/thinking/checkpoint", first),
		map[string]string{"text": "我想换一个问题。", "direction": "explore"}, http.StatusOK, nil)
	for _, result := range []chat.Answer{answer, resumed} {
		var record types.AgentRun
		workflowJSON(t, ctx, restarted, http.MethodGet, "/api/runs/"+result.RunID, nil, http.StatusOK, &record)
		var state runtime.RunState
		if err := json.Unmarshal(record.State, &state); err != nil {
			t.Fatal(err)
		}
		if state.Status != runtime.RunCompleted || !reflect.DeepEqual(state.Thinking, result.Thinking) {
			t.Fatalf("persistent thinking run does not match the response: %+v", state)
		}
		if result.RunID == answer.RunID && (len(state.Steps) != 2 || state.Steps[0].ToolName != ability.SearchMemoriesToolName || state.Steps[1].ToolName != ability.GetMessagesToolName) {
			t.Fatalf("lost search and original lookup after restart: %+v", state.Steps)
		}
	}
}

func assertThinkingTranscript(t *testing.T, ctx context.Context, db *gorm.DB, server *httptest.Server, conversationID uint, question string, answer chat.Answer) {
	t.Helper()
	var history workflowHistory
	workflowJSON(t, ctx, server, http.MethodGet, fmt.Sprintf("/api/conversations/%d/messages", conversationID), nil, http.StatusOK, &history)
	if len(history.Messages) != 2 || history.Messages[0].Role != "user" || history.Messages[0].Content != question ||
		history.Messages[1].Role != "assistant" || history.Messages[1].Content != answer.Text {
		t.Fatalf("raw thinking conversation lost speakers or text: %+v", history)
	}
	if !history.Messages[0].Thinking || !history.Messages[1].Thinking || !reflect.DeepEqual(history.Messages[0].ThinkingContext, answer.Thinking) || history.Messages[1].ThinkingContext != nil {
		t.Fatalf("thinking provenance lost after loading original messages: %+v", history)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var session types.Session
		if err := db.WithContext(ctx).First(&session, history.SessionID).Error; err != nil {
			t.Fatal(err)
		}
		if session.ExtractStatus == types.ExtractDone {
			break
		}
		if session.ExtractStatus == types.ExtractFailed {
			t.Fatalf("thinking extraction failed: %s", session.ExtractError)
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for thinking extraction")
		case <-ticker.C:
		}
	}
	var memories []types.Memory
	if err := db.WithContext(ctx).Where("session_id = ?", history.SessionID).Find(&memories).Error; err != nil {
		t.Fatal(err)
	}
	if answer.Thinking.Navigation {
		if len(memories) != 0 {
			t.Fatalf("automatic navigation or assistant reply indexed as a user thought: %+v", memories)
		}
		return
	}
	if len(memories) != 1 || memories[0].SeqStart != 1 || memories[0].SeqEnd != 1 || memories[0].Content != question || memories[0].ContentType != types.ContentOther {
		t.Fatalf("global memories must contain only the user's original words: %+v", memories)
	}
}
