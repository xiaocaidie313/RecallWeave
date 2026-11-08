package parser

import (
	"archive/zip"
	"bytes"
	"reflect"
	"strings"
	"testing"

	"recallweave/internal/types"
)

func TestParseMarkdownDocumentPreservesSections(t *testing.T) {
	sections := []string{
		"# RecallWeave\n\n项目说明。",
		"## 数据结构\n\n- 第一项\n\n  列表中的第二段\n\n- 第二项\n\n| 字段 | 类型 |\n| --- | --- |\n| ID | uint |",
		"## 示例\n\n```go\nfunc demo() {\n\n    println(\"hello\")\n}\n```",
	}
	raw := strings.Join(sections, "\n\n")
	conversations, err := ParseWithFormat(raw, types.SourceCodex, FormatMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].Title != "RecallWeave" {
		t.Fatalf("unexpected conversations: %+v", conversations)
	}
	assertMarkdownMessages(t, conversations[0].Messages, types.SourceCodex, []string{"user", "user", "user"}, sections)
}

func TestParseMarkdownChatKeepsRolesAndNestedBlocks(t *testing.T) {
	answer := "可以使用如下代码：\n\n````markdown\n## User\n\n## Assistant\n```\n````\n\n### 解释\n\n> ## User\n> 这只是引用。\n\n- 第一项\n\n- 第二项"
	raw := "# 项目讨论\n\n## **User**:\n\n请解释接口。\n\n## Assistant\n\n" + answer + "\n\n## 用户\n\n谢谢。\n\n## 助手\n\n不客气。\n"
	conversations, err := ParseMarkdown(raw, types.SourceChatGPT)
	if err != nil {
		t.Fatal(err)
	}
	if conversations[0].Title != "项目讨论" {
		t.Fatalf("title lost: %q", conversations[0].Title)
	}
	assertMarkdownMessages(t, conversations[0].Messages, types.SourceChatGPT,
		[]string{"user", "assistant", "user", "assistant"}, []string{"请解释接口。", answer, "谢谢。", "不客气。"})
}

func TestParseMarkdownSetextChatHeadings(t *testing.T) {
	raw := "项目讨论\n========\n\nUser\n----\n\n问题。\n\nAssistant\n---------\n\n回答。\n"
	conversations, err := ParseMarkdown(raw, types.SourceChatGPT)
	if err != nil {
		t.Fatal(err)
	}
	if conversations[0].Title != "项目讨论" {
		t.Fatalf("unexpected title: %q", conversations[0].Title)
	}
	assertMarkdownMessages(t, conversations[0].Messages, types.SourceChatGPT, []string{"user", "assistant"}, []string{"问题。", "回答。"})
}

func TestParseMarkdownConservativeRoleDetection(t *testing.T) {
	cases := map[string]string{
		"single role chapter":      "# 文档\n\n## Assistant\n\n关于助手的说明。",
		"different heading levels": "# 文档\n\n## User\n\n用户说明。\n\n### Assistant\n\n助手说明。",
		"similar chapter names":    "# 文档\n\n## 用户管理\n\n管理流程。\n\n## 助手配置\n\n配置流程。",
		"fenced role pair":         "# 文档\n\n~~~markdown\n## User\n\n## Assistant\n~~~",
		"indented role pair":       "    ## User\n\n    ## Assistant",
		"quoted role pair":         "> ## User\n> 内容。\n>\n> ## Assistant\n> 内容。",
		"list role pair":           "- ## User\n\n- ## Assistant",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			conversations, err := ParseMarkdown(raw, types.SourceCodex)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range conversations[0].Messages {
				if message.Role != "user" {
					t.Fatalf("document misclassified as chat: %+v", message)
				}
			}
			var parts []string
			for _, message := range conversations[0].Messages {
				parts = append(parts, message.Content)
			}
			if strings.Join(parts, "\n\n") != raw {
				t.Fatalf("document content changed: %q", parts)
			}
		})
	}
}

func TestParseMarkdownKeepsPreambleAndNormalizesWindowsText(t *testing.T) {
	raw := "\ufeff# 讨论\r\n\r\n来源说明。\r\n\r\n## You said:\r\n\r\n    indented code\r\n\r\n    second line\r\n\r\n## ChatGPT said:\r\n\r\n第一行  \r\n第二行\r\n"
	conversations, err := ParseMarkdown(raw, types.SourceChatGPT)
	if err != nil {
		t.Fatal(err)
	}
	assertMarkdownMessages(t, conversations[0].Messages, types.SourceChatGPT,
		[]string{"user", "user", "assistant"}, []string{"# 讨论\n\n来源说明。", "    indented code\n\n    second line", "第一行  \n第二行"})
}

func TestParseMarkdownNoHeadingsKeepsWholeDocument(t *testing.T) {
	raw := "第一段。\n\n第二段。\n\n```go\n\nprintln(1)\n```"
	conversations, err := ParseMarkdown(raw, types.SourceWX)
	if err != nil {
		t.Fatal(err)
	}
	assertMarkdownMessages(t, conversations[0].Messages, types.SourceWX, []string{"user"}, []string{raw})
}

func TestParseMarkdownEmptyHeadingsAndSetextSections(t *testing.T) {
	for _, raw := range []string{"#\n\n##\n", "### ###\n\n正文。", "文档\n====\n\n章节\n----\n\n正文。"} {
		t.Run(raw, func(t *testing.T) {
			conversations, err := ParseMarkdown(raw, types.SourceCodex)
			if err != nil {
				t.Fatal(err)
			}
			assertMarkdownMessages(t, conversations[0].Messages, types.SourceCodex, []string{"user"}, []string{strings.TrimRight(raw, "\n")})
		})
	}
}

func TestParseMarkdownRejectsEmptyContentAndInvalidUTF8(t *testing.T) {
	for _, raw := range []string{"", "\ufeff \r\n\t", string([]byte{0xff, 0xfe}), "## User\n\n## Assistant\n"} {
		if _, err := ParseMarkdown(raw, types.SourceChatGPT); err == nil {
			t.Fatalf("expected unreadable markdown to fail: %q", raw)
		}
	}
}

func TestParseWithFormatPreservesDefaultAndRejectsUnknown(t *testing.T) {
	raw := "first\n\nsecond"
	got, err := ParseWithFormat(raw, types.SourceWX, FormatAuto)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse(raw, types.SourceWX)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("default parser changed: got %+v, want %+v, err %v", got, want, err)
	}
	if _, err := ParseWithFormat(raw, types.SourceWX, Format("pdf")); err == nil {
		t.Fatal("unsupported format silently accepted")
	}
	if _, err := Parse("# Markdown", types.SourceChatGPT); err == nil {
		t.Fatal("default ChatGPT parser must still expect official JSON")
	}
}

func TestReadFileWithFormatDetectsMarkdown(t *testing.T) {
	raw := "# 导出\n\n## User\n\n问题\n\n## Assistant\n\n回答"
	for _, filename := range []string{"export.md", "export.MD", "export.markdown", "export.MARKDOWN"} {
		t.Run(filename, func(t *testing.T) {
			got, format, err := ReadFileWithFormat([]byte(raw), filename)
			if err != nil || got != raw || format != FormatMarkdown {
				t.Fatalf("markdown format lost: text=%q format=%q err=%v", got, format, err)
			}
		})
	}
	got, format, err := ReadFileWithFormat([]byte(raw), "export.txt")
	if err != nil || got != raw || format != FormatAuto {
		t.Fatalf("plain text should retain default format: %q, %q, %v", got, format, err)
	}
}

func TestReadFileWithFormatDetectsZIPEntryMarkdown(t *testing.T) {
	raw := "# 导出\n\n## User\n\n问题\n\n## Assistant\n\n回答"
	for _, name := range []string{"nested/export.md", "nested/export.MARKDOWN"} {
		data := markdownZIP(t, []zipText{{name, raw}})
		got, format, err := ReadFileWithFormat(data, "archive.zip")
		if err != nil || got != raw || format != FormatMarkdown {
			t.Fatalf("ZIP inner format lost: text=%q format=%q err=%v", got, format, err)
		}
		parsed, err := ParseWithFormat(got, types.SourceChatGPT, format)
		if err != nil {
			t.Fatal(err)
		}
		assertMarkdownMessages(t, parsed[0].Messages, types.SourceChatGPT, []string{"user", "assistant"}, []string{"问题", "回答"})
		legacy, err := ReadFile(data)
		if err != nil || legacy != raw {
			t.Fatalf("legacy file reader changed: %q, %v", legacy, err)
		}
	}
}

func TestReadFileWithFormatPreservesZIPJSONPreference(t *testing.T) {
	raw := `[{"title":"json","mapping":{}}]`
	data := markdownZIP(t, []zipText{{"first.md", "# Markdown"}, {"nested/conversations.json", raw}})
	got, format, err := ReadFileWithFormat(data, "archive.zip")
	if err != nil || got != raw || format != FormatAuto {
		t.Fatalf("official JSON should still win: %q, %q, %v", got, format, err)
	}
}

func assertMarkdownMessages(t *testing.T, messages []types.Message, source types.SourceTag, roles, contents []string) {
	t.Helper()
	if len(messages) != len(contents) {
		t.Fatalf("message count: got %d want %d: %+v", len(messages), len(contents), messages)
	}
	for i, message := range messages {
		if message.Role != roles[i] || message.Content != contents[i] || message.Seq != i+1 || message.SourceTag != source {
			t.Fatalf("message %d: got %+v, want role=%q content=%q seq=%d source=%q", i, message, roles[i], contents[i], i+1, source)
		}
	}
}

type zipText struct{ name, text string }

func markdownZIP(t *testing.T, files []zipText) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range files {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(file.text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
