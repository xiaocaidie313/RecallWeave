package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"recallweave/internal/parser"
	"recallweave/internal/store"
)

type goldMemory struct {
	SeqStart int
	SeqEnd   int
	Title    string
	Content  string
}

var goldMemories = []goldMemory{
	{1, 2, "后端用 gin", "后端框架选定 gin，不用 echo"},
	{3, 4, "迁到 PostgreSQL", "数据库从 MySQL 迁到 PostgreSQL，端口 5434"},
	{5, 6, "杭州去北京", "住在杭州，下周去北京开会"},
	{7, 8, "cmd 是入口", "cmd 目录是服务入口，不要塞业务"},
	{9, 10, "工具六轮", "问答工具轮次上限是六轮"},
}

func TestChatGPTFixtureHitsExpectedSpans(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "testdata", "chatgpt_eval.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	conversations, err := parser.Parse(string(raw), store.SourceChatGPT)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(conversations) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(conversations))
	}

	messages := conversations[0].Messages
	if len(messages) != 10 {
		t.Fatalf("expected 10 messages, got %d", len(messages))
	}

	bySeq := make(map[int]store.Message, len(messages))
	for _, msg := range messages {
		bySeq[msg.Seq] = msg
	}
	memories := make([]store.Memory, 0, len(goldMemories))
	for _, gold := range goldMemories {
		if _, ok := bySeq[gold.SeqStart]; !ok {
			t.Fatalf("fixture missing seq %d", gold.SeqStart)
		}
		if _, ok := bySeq[gold.SeqEnd]; !ok {
			t.Fatalf("fixture missing seq %d", gold.SeqEnd)
		}
		memories = append(memories, store.Memory{
			SessionID: 1,
			SeqStart:  gold.SeqStart,
			SeqEnd:    gold.SeqEnd,
			Title:     gold.Title,
			Content:   gold.Content,
		})
	}

	cases := []struct {
		question string
		seqStart int
		seqEnd   int
	}{
		{"后端 gin", 1, 2},
		{"PostgreSQL", 3, 4},
		{"杭州", 5, 6},
		{"cmd 目录", 7, 8},
		{"工具轮次", 9, 10},
	}

	for _, tc := range cases {
		hits := matchMemories(memories, tc.question)
		if !covers(hits, tc.seqStart, tc.seqEnd) {
			t.Errorf("question %q should hit seq %d-%d, got %+v", tc.question, tc.seqStart, tc.seqEnd, hits)
		}
	}
}

func matchMemories(memories []store.Memory, keyword string) []store.Memory {
	tokens := strings.Fields(keyword)
	var hits []store.Memory
	for _, item := range memories {
		if len(tokens) == 0 {
			hits = append(hits, item)
			continue
		}
		for _, token := range tokens {
			if strings.Contains(item.Title, token) || strings.Contains(item.Content, token) {
				hits = append(hits, item)
				break
			}
		}
	}
	return hits
}

func covers(hits []store.Memory, seqStart, seqEnd int) bool {
	for _, hit := range hits {
		if hit.SessionID == 1 && hit.SeqStart <= seqStart && hit.SeqEnd >= seqEnd {
			return true
		}
	}
	return false
}
