package parser

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/types"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// Format 描述导入文本的语法，不改变来源平台 SourceTag。
type Format string

const (
	FormatAuto     Format = ""
	FormatMarkdown Format = "markdown"
)

func (f Format) Valid() bool { return f == FormatAuto || f == FormatMarkdown }

type markdownHeading struct {
	start int
	end   int
	level int
	label string
	role  string
}

// ParseMarkdown 保留原始 Markdown 片段，不渲染 HTML。
// 普通文档按章节拆分；聊天导出需在同一标题层级同时出现用户和助手角色。
// 只检查文档根节点上的标题，代码块、列表和引用里的标题不参与消息分隔。
func ParseMarkdown(raw string, sourceTag types.SourceTag) ([]Conversation, error) {
	if !utf8.ValidString(raw) {
		return nil, fmt.Errorf("markdown is not valid UTF-8")
	}
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("markdown has no importable content")
	}

	source := []byte(raw)
	document := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(source))
	var headings []markdownHeading
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		if heading, ok := node.(*ast.Heading); ok {
			headings = append(headings, markdownHeadingFromNode(heading, source))
		}
	}

	roleLevel := markdownRoleLevel(headings)
	title := ""
	for _, heading := range headings {
		if roleLevel > 0 && heading.level == roleLevel && heading.role != "" {
			break
		}
		if heading.level == 1 && heading.label != "" {
			title = heading.label
			break
		}
	}

	var messages []types.Message
	if roleLevel > 0 {
		messages = markdownChatMessages(raw, headings, roleLevel, title, sourceTag)
	} else {
		messages = markdownDocumentMessages(raw, source, document, sourceTag)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("markdown has no importable content")
	}
	return []Conversation{{Title: title, Messages: messages}}, nil
}

func markdownHeadingFromNode(node *ast.Heading, source []byte) markdownHeading {
	start := node.Pos()
	if start < 0 {
		start = node.Lines().At(0).Start
	}
	start = bytes.LastIndexByte(source[:start], '\n') + 1
	end := markdownLineEnd(source, start)
	// Setext 标题可以跨多行，末尾还有一行 === / --- 下划线。
	if !bytes.HasPrefix(bytes.TrimSpace(source[start:end]), []byte("#")) {
		if n := node.Lines().Len(); n > 0 {
			end = markdownLineEnd(source, node.Lines().At(n-1).Stop)
		}
		end = markdownLineEnd(source, end)
	}
	var label strings.Builder
	_ = ast.Walk(node, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch inline := child.(type) {
			case *ast.Text:
				label.Write(inline.Value(source))
				if inline.SoftLineBreak() || inline.HardLineBreak() {
					label.WriteByte(' ')
				}
			case *ast.String:
				label.Write(inline.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	value := strings.TrimSpace(label.String())
	return markdownHeading{start: start, end: end, level: node.Level, label: value, role: markdownRole(value)}
}

func markdownLineEnd(source []byte, start int) int {
	if offset := bytes.IndexByte(source[start:], '\n'); offset >= 0 {
		return start + offset + 1
	}
	return len(source)
}

func markdownRole(label string) string {
	label = strings.ToLower(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(label), ":：")))
	switch label {
	case "user", "human", "you", "you said", "用户", "你说", "您说":
		return "user"
	case "assistant", "chatgpt", "chatgpt said", "claude", "助手", "ai":
		return "assistant"
	default:
		return ""
	}
}

func markdownRoleLevel(headings []markdownHeading) int {
	var users, assistants [7]bool
	for _, heading := range headings {
		if heading.role == "user" {
			users[heading.level] = true
		} else if heading.role == "assistant" {
			assistants[heading.level] = true
		}
	}
	for level := 1; level <= 6; level++ {
		if users[level] && assistants[level] {
			return level
		}
	}
	return 0
}

func markdownChatMessages(raw string, headings []markdownHeading, level int, title string, sourceTag types.SourceTag) []types.Message {
	var markers []markdownHeading
	for _, heading := range headings {
		if heading.level == level && heading.role != "" {
			markers = append(markers, heading)
		}
	}
	var messages []types.Message
	preamble := trimMarkdownBlock(raw[:markers[0].start])
	// 只有文档标题时由 Session.Name 保存，不虚构一条用户发言。
	if title != "" {
		for _, heading := range headings {
			if heading.level == 1 && heading.label == title && heading.start < markers[0].start &&
				preamble == trimMarkdownBlock(raw[heading.start:heading.end]) {
				preamble = ""
				break
			}
		}
	}
	messages = appendMarkdownMessage(messages, sourceTag, "user", preamble)
	for i, marker := range markers {
		end := len(raw)
		if i+1 < len(markers) {
			end = markers[i+1].start
		}
		messages = appendMarkdownMessage(messages, sourceTag, marker.role, raw[marker.end:end])
	}
	return messages
}

func markdownDocumentMessages(raw string, source []byte, document ast.Node, sourceTag types.SourceTag) []types.Message {
	var messages []types.Message
	start, hasBody := 0, false
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		if heading, ok := node.(*ast.Heading); ok {
			if hasBody {
				boundary := markdownHeadingFromNode(heading, source).start
				messages = appendMarkdownMessage(messages, sourceTag, "user", raw[start:boundary])
				start, hasBody = boundary, false
			}
		} else {
			hasBody = true
		}
	}
	return appendMarkdownMessage(messages, sourceTag, "user", raw[start:])
}

func appendMarkdownMessage(messages []types.Message, sourceTag types.SourceTag, role, content string) []types.Message {
	content = trimMarkdownBlock(content)
	if content == "" {
		return messages
	}
	return append(messages, types.Message{Seq: len(messages) + 1, Role: role, SourceTag: sourceTag, Content: content})
}

// 只去首尾空白行，保留缩进代码的前导空格、硬换行和块内空行。
func trimMarkdownBlock(raw string) string {
	lines := strings.Split(raw, "\n")
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}
