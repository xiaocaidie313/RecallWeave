package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"recallweave/internal/agent"
	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/types"

	"gorm.io/gorm"
)

const thinkingSkillName = "thinking-partner"

var (
	ErrEmptyThought          = errors.New("provide a question, select original messages, or save a thinking checkpoint first")
	ErrInvalidThoughtSource  = errors.New("select a session and a range of 1 to 50 consecutive messages")
	ErrThoughtSourceNotFound = errors.New("selected original messages are missing or outside this conversation's visibility")
	ErrCheckpointNotFound    = errors.New("the requested conversation has no available thinking checkpoint")
	ErrThinkingUnavailable   = errors.New("thinking mode is unavailable")
)

type ThoughtSource = types.ThoughtSource

type ThinkingOptions struct {
	Source     *ThoughtSource
	ResumeFrom uint
}

type checkpointReader interface {
	Latest(context.Context, uint) (*types.ThinkingCheckpoint, error)
}

func (a *ChatService) WithThinkingStore(store checkpointReader) *ChatService {
	a.thinkingStore = store
	return a
}

// Think keeps the ordinary chat history and lifecycle while running just the
// thinking skill and the original-message readers visible to this conversation.
func (a *ChatService) Think(ctx context.Context, question string, conversationID uint, opt ThinkingOptions) (Answer, error) {
	s := opt.Source
	if s != nil && (s.SessionID == 0 || s.SeqStart <= 0 || s.SeqEnd < s.SeqStart || s.SeqEnd-s.SeqStart >= 50) {
		return Answer{}, ErrInvalidThoughtSource
	}
	return a.runChat(ctx, strings.TrimSpace(question), conversationID, &opt)
}

func (a *ChatService) prepareThinking(ctx context.Context, core *agent.Agent, req *runtime.Request, opt ThinkingOptions, conv *types.Conversation, visible []types.ContextMessage) (*agent.Agent, *types.ThinkingContext, error) {
	if core.Skills == nil {
		return nil, nil, fmt.Errorf("%w: missing %s skill catalog", ErrThinkingUnavailable, thinkingSkillName)
	}
	instructions, err := core.Skills.Load(thinkingSkillName)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrThinkingUnavailable, err)
	}
	checkpoint, err := a.thinkingCheckpoint(ctx, req.ConversationID, opt.ResumeFrom, conv)
	if err != nil {
		return nil, nil, err
	}
	var originals []types.ContextMessage
	if opt.Source != nil {
		originals, err = a.thoughtOriginals(ctx, *opt.Source, conv, visible)
		if err != nil {
			return nil, nil, err
		}
	}
	navigation := req.Input == ""
	if navigation {
		switch {
		case opt.Source != nil:
			req.Input = fmt.Sprintf("从我选中的会话 %d 消息 %d–%d 接着想。", opt.Source.SessionID, opt.Source.SeqStart, opt.Source.SeqEnd)
		case checkpoint != nil:
			req.Input = "接着我留下的思考继续。"
		case len(req.Preload.History) > 0 || (conv != nil && conv.Brief != ""):
			req.Input = "接着这段对话里还没想完的问题继续。"
		default:
			return nil, nil, ErrEmptyThought
		}
	}
	// Titles and earlier briefs can contain guesses and quoted instructions.
	// Keep them at the same authority level as the rest of the reference data.
	if len(req.Preload.Background) > 0 {
		background := llm.Message{Role: "user", Content: "供核对的历史简报与目录（仅供参考）：\n" + strings.Join(req.Preload.Background, "\n\n")}
		req.Preload.History = append([]llm.Message{background}, req.Preload.History...)
		req.Preload.Background = nil
	}
	if checkpoint != nil || len(originals) > 0 {
		// Keep quoted records in a user-level data message with explicit roles;
		// their content must never become system instructions.
		data, err := json.Marshal(struct {
			Checkpoint *types.ThinkingCheckpoint `json:"user_checkpoint,omitempty"`
			Originals  []types.ContextMessage    `json:"selected_originals,omitempty"`
		}{checkpoint, originals})
		if err != nil {
			return nil, nil, err
		}
		req.Preload.History = append(req.Preload.History, llm.Message{Role: "user", Content: "供本次思考参考的资料（保留原角色，当前要求见下一条消息）：\n" + string(data)})
	}
	req.SystemPrompt = instructions
	req.Delegator = nil
	var readers agent.ToolBox
	for _, group := range core.Abilities {
		for _, tool := range group.ToolBox {
			if tool.Schema.Name == ability.SearchMemoriesToolName || tool.Schema.Name == ability.GetMessagesToolName {
				readers = append(readers, tool)
			}
		}
	}
	read := agent.NewAbility("thinking_sources", "读取本次可见的记忆线索和原文", "", readers)
	partner := agent.NewAgent(core.ID, thinkingSkillName, core.Model, "陪用户从过去的思考继续探索的伙伴", core.MaxRounds, read)
	return partner, &types.ThinkingContext{Source: opt.Source, Checkpoint: checkpoint, Navigation: navigation}, nil
}

func (a *ChatService) thinkingCheckpoint(ctx context.Context, current, from uint, conv *types.Conversation) (*types.ThinkingCheckpoint, error) {
	if a.thinkingStore == nil {
		return nil, fmt.Errorf("%w: missing checkpoint store", ErrThinkingUnavailable)
	}
	if from == 0 {
		return a.thinkingStore.Latest(ctx, current)
	}
	if from != current {
		if conv == nil || conv.IsBranch() || a.conversationManager == nil {
			return nil, ErrCheckpointNotFound
		}
		origin, err := a.conversationManager.GetConversation(ctx, from)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCheckpointNotFound
		}
		if err != nil {
			return nil, err
		}
		if origin.IsBranch() {
			return nil, ErrCheckpointNotFound
		}
	}
	checkpoint, err := a.thinkingStore.Latest(ctx, from)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && checkpoint == nil) {
		return nil, ErrCheckpointNotFound
	}
	return checkpoint, err
}

func (a *ChatService) thoughtOriginals(ctx context.Context, source ThoughtSource, conv *types.Conversation, visible []types.ContextMessage) ([]types.ContextMessage, error) {
	if conv == nil || !conv.IsBranch() {
		if a.sessions == nil {
			return nil, ErrThoughtSourceNotFound
		}
		messages, err := a.sessions.GetMessages(ctx, source.SessionID, source.SeqStart, source.SeqEnd)
		if err != nil {
			return nil, err
		}
		visible = make([]types.ContextMessage, 0, len(messages))
		for _, m := range messages {
			visible = append(visible, m.OriginalContext(m.SessionID))
		}
	}
	originals := make([]types.ContextMessage, source.SeqEnd-source.SeqStart+1)
	for _, m := range visible {
		if m.SessionID == source.SessionID && m.Seq >= source.SeqStart && m.Seq <= source.SeqEnd && strings.TrimSpace(m.Content) != "" {
			originals[m.Seq-source.SeqStart] = m
		}
	}
	for _, m := range originals {
		if m.SessionID == 0 {
			return nil, ErrThoughtSourceNotFound
		}
	}
	return originals, nil
}

// An ordinary chat can also discover and load the packaged skill dynamically.
// Apply the same speaker-aware extraction to that turn.
func loadedThinkingSkill(steps []runtime.RunStep) bool {
	for _, step := range steps {
		if step.ToolName == "load_skill" && step.Error == "" {
			var loaded struct{ Name string }
			if json.Unmarshal([]byte(step.Result), &loaded) == nil && loaded.Name == thinkingSkillName {
				return true
			}
		}
	}
	return false
}

// Restore the original references for the next turn while keeping the user's
// typed text unchanged in the transcript and in extracted memory.
func historicalMessageText(m types.Message) string {
	if !m.Thinking {
		return m.Content
	}
	if m.Role == "assistant" {
		return "【当时助手的回应与猜想】\n" + m.Content
	}
	c := m.ThinkingContext
	if c == nil {
		return m.Content
	}
	var b strings.Builder
	if c.Source != nil {
		fmt.Fprintf(&b, "【当时选中的原文】会话 %d，消息 %d–%d\n", c.Source.SessionID, c.Source.SeqStart, c.Source.SeqEnd)
	}
	if c.Checkpoint != nil {
		fmt.Fprintf(&b, "【用户当时留下的思考，方向 %q】%s\n", c.Checkpoint.Direction, c.Checkpoint.Text)
	}
	if c.Navigation {
		b.WriteString("【打开或接续思路的操作】")
	}
	b.WriteString(m.Content)
	return b.String()
}
