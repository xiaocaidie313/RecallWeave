package ask

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
	"recallweave/internal/store"
)

// preloadedMemoriesLimit 混合检索预取的记忆条数。只放几条摘要进上下文，
// 控制住 token 成本，细节让模型用工具自己挖。
const preloadedMemoriesLimit = 4

const (
	maxCatalogPerSession = 8
	maxCatalogTotal      = 24
)

// loadContext 准备这次提问需要的全部背景，并返回对应的 chat session。
// 分三层：
//  1. 记忆层：relatedMemories 从全库检索与问题相关的记忆（混合检索）。
//  2. 导入层：挂到这次对话上的会话，把记忆目录放进背景。
//  3. 对话层：本对话的历史消息 + 之前的简报。
//
// 只作知识背景的导入不挂 conversation，不会出现在导入层，仍走第 1 层。
// 没有记忆库或没有 conversation 时跳过，测试可以不连数据库。
func (a *Agent) loadContext(ctx context.Context, conversationID uint, question string) (runtime.Preload, uint, error) {
	var preload runtime.Preload
	if a.memory == nil || conversationID == 0 {
		return preload, 0, nil
	}

	if background := a.relatedMemories(ctx, question); background != "" {
		preload.Background = append(preload.Background, background)
	}

	imported, err := a.importedBackground(ctx, conversationID)
	if err != nil {
		return preload, 0, err
	}
	if imported != "" {
		preload.Background = append(preload.Background, imported)
	}

	session, err := a.memory.ChatSession(ctx, conversationID)
	if err != nil {
		return preload, 0, err
	}

	brief, untilSeq, err := a.memory.ConversationBrief(ctx, conversationID)
	if err != nil {
		return preload, 0, err
	}
	if brief != "" {
		preload.Background = append(preload.Background, "此前对话摘要：\n"+brief)
	}

	messages, err := a.memory.ListMessages(ctx, session.ID)
	if err != nil {
		return preload, 0, err
	}
	for _, msg := range messages {
		if untilSeq > 0 && msg.Seq <= untilSeq {
			continue
		}
		preload.History = append(preload.History, llm.Message{
			Seq:     msg.Seq,
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	return preload, session.ID, nil
}

// relatedMemories 用当前问题从记忆库检索相关记忆，把 top-K 的标题+摘要拼成一段背景。
// 这些预取的记忆不进 citations——它们是「可能的背景」，真正的引用依据走工具链。
// 检索失败只记日志，不让预取失败拦截掉这一次提问。
func (a *Agent) relatedMemories(ctx context.Context, question string) string {
	if question == "" {
		return ""
	}
	hits, err := a.memory.SearchMemories(ctx, question, "", preloadedMemoriesLimit)
	if err != nil {
		slog.Warn("preload memories failed", "error", err)
		return ""
	}
	if len(hits) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("用户过去可能相关的经历：\n")
	for _, hit := range hits {
		builder.WriteString("- ")
		builder.WriteString(hit.Title)
		builder.WriteString("\n  ")
		builder.WriteString(hit.Content)
		builder.WriteByte('\n')
	}
	builder.WriteString("\n这些仅供参考，回答时如果你不确定，用 search_memories 或 get_messages 核实。")
	return builder.String()
}

type importedPiece struct {
	session   store.Session
	memories  []store.Memory
	truncated bool
}

// importedBackground 列出挂在这次对话上的导入会话。
// 助手自己的 chat 会话跳过，它的原文已经在对话层。
func (a *Agent) importedBackground(ctx context.Context, conversationID uint) (string, error) {
	sessions, err := a.memory.GetSessions(ctx, conversationID)
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
		if session.SourceTag == store.SourceChat {
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

func truncateMemories(memories []store.Memory, remaining int) (taken []store.Memory, left int, truncated bool) {
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

func formatSessionHeading(session store.Session) string {
	name := strings.TrimSpace(session.Name)
	if name == "" {
		return fmt.Sprintf("- 会话 %d「未命名」（%s）\n", session.ID, session.SourceTag)
	}
	return fmt.Sprintf("- 会话 %d「%s」（%s）\n", session.ID, name, session.SourceTag)
}
