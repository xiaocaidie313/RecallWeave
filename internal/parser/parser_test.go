package parser

import (
	"os"
	"path/filepath"
	"testing"

	"recallweave/internal/store"
)

func TestProjectDecisionFixturePreservesAttribution(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "testdata", "project-decisions.json"))
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := Parse(string(data), store.SourceChatGPT)
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := [][]string{{"user", "assistant", "user"}, {"assistant", "user", "assistant"}, {"user", "assistant", "user"}}
	if len(conversations) != len(wantRoles) {
		t.Fatalf("expected %d source sessions, got %d", len(wantRoles), len(conversations))
	}
	for i, roles := range wantRoles {
		messages := conversations[i].Messages
		if len(messages) != len(roles) {
			t.Fatalf("session %d: unexpected message count %d", i, len(messages))
		}
		for j, role := range roles {
			if messages[j].Role != role || messages[j].Seq != j+1 || messages[j].Content == "" {
				t.Fatalf("session %d message %d: attribution lost: %+v", i, j, messages[j])
			}
		}
	}
}

func TestParseChatGPTKeepsRoles(t *testing.T) {
	raw := `{
		"title": "问候",
		"current_node": "c",
		"mapping": {
			"a": {"parent": null, "children": ["b"], "message": {"author": {"role": "system"}, "content": {"parts": ["你是助手"]}}},
			"b": {"parent": "a", "children": ["c"], "message": {"author": {"role": "user"}, "content": {"parts": ["你好"]}}},
			"c": {"parent": "b", "children": [], "message": {"author": {"role": "assistant"}, "content": {"parts": ["你好，我是助手"]}}}
		}
	}`

	conversations, err := Parse(raw, store.SourceChatGPT)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(conversations) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(conversations))
	}
	if conversations[0].Title != "问候" {
		t.Fatalf("unexpected title: %q", conversations[0].Title)
	}

	messages := conversations[0].Messages
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "你好" {
		t.Fatalf("unexpected first message: %+v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "你好，我是助手" {
		t.Fatalf("unexpected second message: %+v", messages[1])
	}
}
