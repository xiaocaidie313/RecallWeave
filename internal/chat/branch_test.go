package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
	"recallweave/internal/memory"
	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestBranchCoreDoesNotMutateSharedAgent(t *testing.T) {
	var globalCalls int
	box := agent.ToolBox{}
	for _, name := range []string{"search_memories", "get_messages", "get_sessions", "deactivate_memory"} {
		box = append(box, llm.Tool{Schema: llm.ToolSchema{Name: name}, Run: func(context.Context, string) (string, error) {
			globalCalls++
			return "global", nil
		}})
	}
	core := agent.NewAgent(1, "recall", nil, "", 6,
		agent.NewAbility("memory", "global memory", "GLOBAL_MEMORY_INSTRUCTIONS", box))
	core.ExternalTools = []llm.Tool{
		{Schema: llm.ToolSchema{Name: "mcp_tavily_search"}},
		{Schema: llm.ToolSchema{Name: "mcp_recall_search_memories"}},
	}
	visible := []store.ContextMessage{{SessionID: 10, Seq: 1, Role: "user", Content: "分叉前"}}
	child := branchCore(core, visible)
	names := []string{}
	for _, schema := range child.Tools().Schemas() {
		names = append(names, schema.Name)
	}
	if !slices.Equal(names, []string{"get_messages", "mcp_tavily_search"}) {
		t.Fatalf("branch exposed global readers or lost public search: %v", names)
	}
	if strings.Contains(child.Instructions(), "GLOBAL_MEMORY_INSTRUCTIONS") {
		t.Fatal("removed global ability instructions leaked into branch")
	}
	result, err := child.Tools().Run(context.Background(), "get_messages", `{"session_id":10,"seq_start":1,"seq_end":2}`)
	if err != nil || !strings.Contains(result, "分叉前") || globalCalls != 0 {
		t.Fatalf("branch read global backend: %q, %v", result, err)
	}
	if _, err := child.Tools().Run(context.Background(), "search_memories", `{}`); err == nil {
		t.Fatal("branch global search must be unavailable")
	}
	if len(core.Abilities[0].ToolBox) != 4 || len(core.ExternalTools) != 2 {
		t.Fatal("shared Agent configuration was mutated")
	}
	if _, err := core.Tools().Run(context.Background(), "search_memories", `{}`); err != nil || globalCalls != 1 {
		t.Fatalf("root memory tools changed: %v", err)
	}
}

func TestBranchLoaderUsesFrozenPrefixWithoutGlobalQueries(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal([]store.ContextMessage{
		{SessionID: 10, Seq: 1, Role: "user", Content: "主线分叉前的要求"},
		{SessionID: 10, Seq: 2, Role: "assistant", Content: "主线分叉前的回复"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// No parent queries, memory queries, embedding calls or imported catalogs are
	// expected. A branch can only load its stored snapshot and its own chat.
	mock.ExpectQuery(`SELECT .* FROM "conversations" WHERE .*`).WithArgs(3, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_conversation_id", "fork_snapshot", "brief", "brief_until_seq"}).
			AddRow(3, 1, string(snapshot), "本支线早期摘要", 1))
	mock.ExpectQuery(`SELECT count\(\*\) FROM "conversations"`).WithArgs(3).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT .* FROM "sessions" WHERE .*`).WithArgs(3, store.SourceChat, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "source_tag"}).AddRow(20, 3, store.SourceChat))
	mock.ExpectQuery(`SELECT .* FROM "messages" WHERE session_id = \$1 ORDER BY seq`).WithArgs(20).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content"}).
			AddRow(1, "user", "本支线已压缩的消息").AddRow(2, "assistant", "本支线近期回复"))
	service := NewAgent(nil, memory.NewMemoryManger(db, nil), nil)
	preload, sessionID, conversation, visible, err := service.loadContext(context.Background(), 3, "现在继续研究")
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != 20 || conversation == nil || !conversation.IsBranch() || len(visible) != 4 || len(preload.History) != 3 {
		t.Fatalf("invalid branch context: %d %+v %+v %+v", sessionID, conversation, visible, preload)
	}
	if preload.History[0].Role != "user" || !strings.Contains(preload.History[0].Content, "会话 10，消息 1") ||
		preload.History[1].Role != "assistant" || !strings.Contains(preload.History[2].Content, "会话 20，消息 2") {
		t.Fatalf("source role or location changed: %+v", preload.History)
	}
	if len(preload.Background) != 1 || !strings.Contains(preload.Background[0], "本支线早期摘要") {
		t.Fatalf("unexpected global/parent background: %+v", preload.Background)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
