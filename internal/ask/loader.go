package ask

import (
	"context"
	"log/slog"
	"strings"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/llm"
)

// preloadedMemoriesLimit 混合检索预取的记忆条数。只放几条摘要进上下文，
// 控制住 token 成本，细节让模型用工具自己挖。
const preloadedMemoriesLimit = 4

// loadContext 准备这次提问需要的全部背景，并返回对应的 chat session。
// 分两个层面：
//  1. 记忆层：relatedMemories 从全库检索与问题相关的记忆（混合检索），
//     模型开答前就知道用户过去的相关经历，不必先学会调工具。
//  2. 对话层：本对话的历史消息 + 之前的简报，模型才知道「这次聊到哪了」。
//
// 记忆层和对话层是两套数据：记忆来自所有导入来源的提炼（微信、GPT、Cursor…），
// 对话是本 conversation 专属的 chat session。别混在一起看。
// 没有记忆库或没有 conversation 时跳过，测试可以不连数据库。
func (a *Agent) loadContext(ctx context.Context, conversationID uint, question string) (runtime.Preload, uint, error) {
	var preload runtime.Preload
	if a.memory == nil || conversationID == 0 {
		return preload, 0, nil
	}

	if background := a.relatedMemories(ctx, question); background != "" {
		preload.Background = append(preload.Background, background)
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
