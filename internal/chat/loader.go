package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/types"
)

const (
	maxCatalogPerSession = 8
	maxCatalogTotal      = 24
)

// loadContext 准备这次提问需要的全部背景，并返回对应的 chat session。
// 只加载本对话的历史、简报，以及明确挂到本对话的导入会话目录。
// 全库记忆由 Agent 在需要历史依据时通过工具检索和核对原文。
//
// 没有会话存储或没有 conversation 时跳过，测试可以不连数据库。
func (a *ChatService) loadContext(ctx context.Context, conversationID uint) (runtime.Preload, uint, *types.Conversation, []types.ContextMessage, error) {
	var preload runtime.Preload
	if a.sessions == nil || conversationID == 0 {
		return preload, 0, nil, nil, nil
	}
	if a.conversationManager == nil {
		return preload, 0, nil, nil, fmt.Errorf("chat: conversation manager is not configured")
	}
	conversation, err := a.conversationManager.GetConversation(ctx, conversationID)
	if err != nil {
		return preload, 0, nil, nil, err
	}
	//判断是否是分支， 分支不加背景
	if !conversation.IsBranch() {
		imported, err := a.importedBackground(ctx, conversationID)
		if err != nil {
			return preload, 0, nil, nil, err
		}
		if imported != "" {
			preload.Background = append(preload.Background, imported)
		}
	}
	//默认 聊天会话的session
	session, err := a.sessions.ChatSession(ctx, conversationID)
	if err != nil {
		return preload, 0, nil, nil, err
	}
	// 子获取父分支的原文
	inheritedMsg, err := conversation.InheritedMessages()
	if err != nil {
		return preload, 0, nil, nil, fmt.Errorf("invalid fork snapshot: %w", err)
	}
	for _, message := range inheritedMsg {
		preload.History = append(preload.History, llm.Message{Seq: message.Seq, Role: message.Role,
			Content: fmt.Sprintf("【继承原文：会话 %d，消息 %d】\n%s", message.SessionID, message.Seq, message.Content)})
	}
	brief, untilSeq := conversation.Brief, conversation.BriefUntilSeq
	if brief != "" {
		preload.Background = append(preload.Background, "此前对话简报：\n"+brief)
	}
	messages, err := a.sessions.ListMessages(ctx, session.ID)
	if err != nil {
		return preload, 0, nil, nil, err
	}
	for _, msg := range messages {
		inheritedMsg = append(inheritedMsg, types.ContextMessage{SessionID: session.ID, Seq: msg.Seq, Role: msg.Role, Content: msg.Content})
		if untilSeq > 0 && msg.Seq <= untilSeq {
			continue
		}
		content := msg.Content
		if conversation.IsBranch() {
			content = fmt.Sprintf("【分支原文：会话 %d，消息 %d】\n%s", session.ID, msg.Seq, msg.Content)
		}
		preload.History = append(preload.History, llm.Message{
			Seq:     msg.Seq,
			Role:    msg.Role,
			Content: content,
		})
	}
	return preload, session.ID, &conversation, inheritedMsg, nil
}

type importedPiece struct {
	session   types.Session
	memories  []types.Memory
	truncated bool
}

// importedBackground 列出挂在这次对话上的导入会话。
// 助手自己的 chat 会话跳过，它的原文已经在对话层。
func (a *ChatService) importedBackground(ctx context.Context, conversationID uint) (string, error) {
	if a.memory == nil {
		return "", nil
	}
	sessions, err := a.sessions.GetSessions(ctx, conversationID)
	if err != nil {
		return "", err
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})
	// 导入原本的session
	pieces := make([]importedPiece, 0, len(sessions))
	remaining := maxCatalogTotal
	for _, session := range sessions {
		//对话session直接跳过
		if session.SourceTag == types.SourceChat {
			continue
		}

		memories, err := a.memory.ListBySession(ctx, session.ID)
		if err != nil {
			return "", err
		}
		piece := importedPiece{session: session}
		piece.memories, remaining, piece.truncated = truncateMemories(memories, remaining)
		if len(piece.memories) == 0 && !piece.truncated {
			continue
		}
		pieces = append(pieces, piece)
	}
	return formatImportedBackground(pieces), nil
}

func truncateMemories(memories []types.Memory, remaining int) (taken []types.Memory, left int, truncated bool) {
	if remaining < 0 {
		remaining = 0
	}
	// 单个session最大条数
	n := maxCatalogPerSession
	// 不超过剩余条数
	if n > remaining {
		n = remaining
	}
	// 不超过记忆条数
	if n > len(memories) {
		n = len(memories)
	}
	//截取 n
	if n > 0 {
		taken = memories[:n]
	}
	return taken, remaining - n, n < len(memories)
}

func formatImportedBackground(pieces []importedPiece) string {
	var catalog strings.Builder // 目录
	truncated := false
	catalogSessions := 0

	for _, piece := range pieces {
		if piece.truncated {
			truncated = true
		}
		if len(piece.memories) == 0 {
			continue
		}
		if catalogSessions == 0 {
			catalog.WriteString("本对话已导入的长期记忆：\n")
		}
		catalogSessions++
		// 写入会话标题
		catalog.WriteString(formatSessionHeading(piece.session))
		// 写入记忆条目
		for _, item := range piece.memories {
			title := item.Title
			if title == "" {
				title = "未命名"
			}
			fmt.Fprintf(&catalog, "  - [%s] %s（memory %d，消息 %d-%d）\n", item.Tag, title, item.ID, item.SeqStart, item.SeqEnd)
		}
	}

	if catalogSessions == 0 {
		return ""
	}

	if truncated {
		catalog.WriteString("目录已截断，其余条目用 search_memories 检索。\n")
	}
	catalog.WriteString("这些是目录，回答时要核实再用 get_messages。")
	return catalog.String()
}

func formatSessionHeading(session types.Session) string {
	name := strings.TrimSpace(session.Name)
	if name == "" {
		return fmt.Sprintf("- 会话 %d「未命名」（%s）\n", session.ID, session.SourceTag)
	}
	return fmt.Sprintf("- 会话 %d「%s」（%s）\n", session.ID, name, session.SourceTag)
}
