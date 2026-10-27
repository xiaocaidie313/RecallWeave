package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"recallweave/internal/agent"
	agentruntime "recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	appmcp "recallweave/internal/mcp"
	"recallweave/internal/skill"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type webScriptModel struct {
	turns []llm.Turn
	next  int
}

func (*webScriptModel) NewOneTurnChat(context.Context, string, string) (string, error) {
	return "", nil
}

func (m *webScriptModel) Next(context.Context, *llm.Conversation, []llm.ToolSchema) (llm.Turn, error) {
	if m.next >= len(m.turns) {
		return llm.Turn{}, errors.New("unexpected extra model call")
	}
	turn := m.turns[m.next]
	m.next++
	return turn, nil
}

type webSearchArgs struct {
	Query             string `json:"query"`
	MaxResults        int    `json:"max_results"`
	SearchDepth       string `json:"search_depth"`
	IncludeRawContent bool   `json:"include_raw_content"`
}

type webSearchResult struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// This uses a real local MCP HTTP connection and scripted model turns. It tests
// integration, not real-model search selection, citation accuracy or privacy.
func TestMemoryAndWebResearchThroughMCP(t *testing.T) {
	catalog, err := skill.Open(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := catalog.Load("web-research")
	if err != nil || body == "" {
		t.Fatalf("packaged web Skill missing: %v", err)
	}
	for _, outcome := range []string{"success", "empty", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			searchArgs := make(chan webSearchArgs, 1)
			var extractCalls atomic.Int32
			server := gomcp.NewServer(&gomcp.Implementation{Name: "mock-tavily", Version: "test"}, nil)
			gomcp.AddTool(server, &gomcp.Tool{Name: "tavily_search", Description: "Search public pages"},
				func(_ context.Context, _ *gomcp.CallToolRequest, args webSearchArgs) (*gomcp.CallToolResult, webSearchResult, error) {
					searchArgs <- args
					if outcome == "failure" {
						return nil, webSearchResult{}, errors.New("mock search unavailable")
					}
					result := webSearchResult{}
					if outcome == "success" {
						if err := json.Unmarshal([]byte(`{"results":[{"title":"pgvector docs","url":"https://docs.example.test/pgvector","content":"Vector index documentation"}]}`), &result); err != nil {
							return nil, result, err
						}
					}
					return nil, result, nil
				})
			gomcp.AddTool(server, &gomcp.Tool{Name: "tavily_extract", Description: "Read public page content"},
				func(_ context.Context, _ *gomcp.CallToolRequest, args struct {
					URLs []string `json:"urls"`
				}) (*gomcp.CallToolResult, map[string]any, error) {
					extractCalls.Add(1)
					if len(args.URLs) != 1 || args.URLs[0] != "https://docs.example.test/pgvector" {
						return nil, nil, errors.New("unexpected extract URL")
					}
					return nil, map[string]any{"results": []map[string]string{{"url": args.URLs[0], "raw_content": "# pgvector documentation\nMock page body."}}}, nil
				})
			gomcp.AddTool(server, &gomcp.Tool{Name: "tavily_crawl"},
				func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
					return nil, struct{}{}, errors.New("crawl must not be exposed")
				})
			handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server },
				&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fake-test-key" || r.URL.RawQuery != "" {
					http.Error(w, "expected bearer authentication without URL credentials", http.StatusUnauthorized)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer httpServer.Close()
			client, err := appmcp.ConnectHTTP(ctx, httpServer.URL, "fake-test-key")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			tools, err := client.AgentTools(ctx, []string{"tavily_search", "tavily_extract"})
			if err != nil {
				t.Fatal(err)
			}
			memory := agent.NewAbility("memory", "Test source memory", "", agent.ToolBox{
				{Schema: llm.ToolSchema{Name: "search_memories"}, Run: func(context.Context, string) (string, error) {
					return `{"memories":[{"memory_id":1,"session_id":3,"seq_start":1,"seq_end":1,"content":"用户要求沿用 PostgreSQL"}]}`, nil
				}},
				{Schema: llm.ToolSchema{Name: "get_messages"}, Run: func(context.Context, string) (string, error) {
					return `{"session_id":3,"messages":[{"seq":1,"role":"user","content":"后端保留 PostgreSQL"}]}`, nil
				}},
			})
			turns := []llm.Turn{
				{ToolCalls: []llm.ToolCall{
					{ID: "skill", Name: "load_skill", Arguments: `{"name":"web-research"}`},
					{ID: "memory", Name: "search_memories", Arguments: `{"keyword":"项目约束"}`},
				}},
				{ToolCalls: []llm.ToolCall{
					{ID: "source", Name: "get_messages", Arguments: `{"session_id":3,"seq_start":1,"seq_end":1}`},
					{ID: "web", Name: "mcp_tavily_search", Arguments: `{"query":"pgvector PostgreSQL official documentation","max_results":5,"search_depth":"basic","include_raw_content":false}`},
				}},
			}
			if outcome == "success" {
				turns = append(turns, llm.Turn{ToolCalls: []llm.ToolCall{{ID: "page", Name: "mcp_tavily_extract", Arguments: `{"urls":["https://docs.example.test/pgvector"]}`}}})
			}
			turns = append(turns, llm.Turn{Content: "Scripted test complete"})
			a := agent.NewAgent(1, "test", &webScriptModel{turns: turns}, "", 5, memory).WithSkills(catalog)
			if err := a.WithExternalTools(tools); err != nil {
				t.Fatal(err)
			}
			for _, schema := range a.Tools().Schemas() {
				if schema.Name == "mcp_tavily_crawl" {
					t.Fatal("tool outside allowlist was exposed")
				}
			}
			result, err := agentruntime.Run(ctx, a, agentruntime.Request{Input: "结合历史约束查阅 pgvector 官方资料"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case args := <-searchArgs:
				if args.Query != "pgvector PostgreSQL official documentation" || args.MaxResults != 5 || args.SearchDepth != "basic" || args.IncludeRawContent {
					t.Fatalf("search arguments changed: %+v", args)
				}
			default:
				t.Fatal("search never reached MCP server")
			}
			steps := result.State.Steps
			if len(steps) < 4 || steps[0].ToolName != "load_skill" || !strings.Contains(steps[0].Result, "web-research") || steps[2].ToolName != "get_messages" || steps[3].ToolName != "mcp_tavily_search" {
				t.Fatalf("expected Skill, memory and external search trace: %+v", steps)
			}
			if outcome == "failure" {
				if !strings.Contains(steps[3].Error, "mock search unavailable") || steps[3].Result != "" {
					t.Fatalf("remote error must not become successful evidence: %+v", steps[3])
				}
			} else {
				var response webSearchResult
				if steps[3].Error != "" || json.Unmarshal([]byte(steps[3].Result), &response) != nil || (len(response.Results) == 0) != (outcome == "empty") {
					t.Fatalf("search outcome lost: %+v", steps[3])
				}
			}
			wantExtract := int32(0)
			if outcome == "success" {
				wantExtract = 1
				if len(steps) != 5 || steps[4].Error != "" || !strings.Contains(steps[4].Result, "Mock page body") {
					t.Fatalf("extract result did not reach Run: %+v", steps)
				}
			}
			if extractCalls.Load() != wantExtract {
				t.Fatalf("unexpected extract calls: %d", extractCalls.Load())
			}
		})
	}
}
