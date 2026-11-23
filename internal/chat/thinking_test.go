package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/skill"
	"recallweave/internal/types"
)

type stubCheckpointReader struct{ checkpoint *types.ThinkingCheckpoint }

func (r stubCheckpointReader) Latest(context.Context, uint) (*types.ThinkingCheckpoint, error) {
	return r.checkpoint, nil
}

func TestThinkingScopesOriginalsAndToolsToBranch(t *testing.T) {
	catalog, err := skill.Open(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	core := agent.NewAgent(1, "recall", nil, "", 6,
		agent.NewAbility("global", "", "", agent.ToolBox{
			{Schema: llm.ToolSchema{Name: "search_memories"}}, {Schema: llm.ToolSchema{Name: "deactivate_memory"}},
		}),
	).WithSkills(catalog)
	parent := uint(2)
	branch := &types.Conversation{ParentConversationID: &parent}
	visible := []types.ContextMessage{
		{SessionID: 9, Seq: 2, Role: "assistant", Content: "我当时的猜测"},
		{SessionID: 9, Seq: 1, Role: "user", Content: "我真正说过的话"},
		{SessionID: 9, Seq: 3, Role: "user", Content: "接着我留下的思考继续。", Navigation: true},
	}
	checkpoint := &types.ThinkingCheckpoint{ConversationID: 3, Text: "我想换个角度", Direction: "reframe", Author: "user"}
	service := NewChatService(core, nil, nil, nil).WithThinkingStore(stubCheckpointReader{checkpoint})
	req := runtime.Request{ConversationID: 3, Preload: runtime.Preload{Background: []string{"旧资料标题"}}}
	opt := ThinkingOptions{Source: &ThoughtSource{SessionID: 9, SeqStart: 1, SeqEnd: 3}}
	scoped := branchCore(core, visible)
	partner, info, err := service.prepareThinking(context.Background(), scoped, &req, opt, branch, visible)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Navigation || info.Checkpoint != checkpoint || info.Source != opt.Source || req.Input == "" {
		t.Fatalf("input context was lost: %+v", info)
	}
	if len(req.Preload.Background) != 0 || len(req.Preload.History) != 2 || req.Preload.History[0].Role != "user" || !strings.Contains(req.Preload.History[0].Content, "旧资料标题") {
		t.Fatalf("background not demoted to quoted data: %+v", req.Preload)
	}
	data := req.Preload.History[1]
	if data.Role != "user" || !strings.Contains(data.Content, `"role":"assistant","content":"我当时的猜测"`) || !strings.Contains(data.Content, `"role":"user","content":"我真正说过的话"`) || !strings.Contains(data.Content, checkpoint.Text) {
		t.Fatalf("role-aware evidence missing: %+v", data)
	}
	if !strings.Contains(data.Content, `"role":"user","content":"接着我留下的思考继续。","navigation":true`) {
		t.Fatalf("explicit source lost its navigation provenance: %+v", data)
	}
	tools := partner.Tools().Schemas()
	if len(tools) != 1 || tools[0].Name != "get_messages" || req.Delegator != nil || len(core.Abilities[0].ToolBox) != 2 {
		t.Fatalf("unexpected branch tools: %+v", tools)
	}
	for _, bad := range []ThoughtSource{{SessionID: 9, SeqStart: 1, SeqEnd: 4}, {SessionID: 99, SeqStart: 1, SeqEnd: 1}} {
		_, err := service.thoughtOriginals(context.Background(), bad, branch, visible)
		if !errors.Is(err, ErrThoughtSourceNotFound) {
			t.Fatalf("outside snapshot %v: %v", bad, err)
		}
	}
	if _, err := service.thinkingCheckpoint(context.Background(), 3, 2, branch); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("branch read foreign checkpoint: %v", err)
	}
}

func TestThinkingRequiresAStartingPoint(t *testing.T) {
	service := newChatService(&fakeChatGenerator{})
	for _, s := range []ThoughtSource{{}, {SessionID: 1, SeqStart: 1, SeqEnd: 51}, {SessionID: 1, SeqStart: 4, SeqEnd: 3}} {
		if _, err := service.Think(context.Background(), "", 7, ThinkingOptions{Source: &s}); !errors.Is(err, ErrInvalidThoughtSource) {
			t.Errorf("invalid source %+v: %v", s, err)
		}
	}
	catalog, err := skill.Open(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	service.core.WithSkills(catalog)
	service.WithThinkingStore(stubCheckpointReader{})
	if _, err := service.Think(context.Background(), "", 7, ThinkingOptions{}); !errors.Is(err, ErrEmptyThought) {
		t.Fatalf("empty question before any thoughts: %v", err)
	}
	if _, err := service.Think(context.Background(), "", 7, ThinkingOptions{ResumeFrom: 7}); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("explicit missing checkpoint: %v", err)
	}
	req := runtime.Request{ConversationID: 7, Preload: runtime.Preload{History: []llm.Message{{Role: "assistant", Content: "还有什么条件能改变你的判断？"}}}}
	_, start, err := service.prepareThinking(context.Background(), service.core, &req, ThinkingOptions{}, nil, nil)
	if err != nil || !start.Navigation || req.Input == "" {
		t.Fatalf("reopen recent discussion: %v %+v", err, start)
	}
}

func TestThinkingSkillMarksOnlySuccessfulLoads(t *testing.T) {
	step := runtime.RunStep{ToolName: "load_skill", Result: `{"name":"thinking-partner"}`}
	if !loadedThinkingSkill([]runtime.RunStep{step}) {
		t.Fatal("successful skill load missed")
	}
	step.Error = "load failed"
	if loadedThinkingSkill([]runtime.RunStep{step}) {
		t.Fatal("failed load marked the turn")
	}
}
