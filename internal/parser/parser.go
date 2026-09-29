package parser

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"recallweave/internal/store"
)

// Conversation 是一次导入里的一段对话。
// ChatGPT 官方导出的一个文件里有很多段，每段各自成为一条会话。
type Conversation struct {
	Title    string
	Messages []store.Message
}

type Parser interface {
	Parse(raw string, sourceTag store.SourceTag) ([]Conversation, error)
	ReadFile(content []byte) (string, error)
	DetectFileType(content []byte) string
}

// ReadFile 把上传的字节读成字符串。zip、json、纯文本都走这里，不解析角色。
func ReadFile(content []byte) (string, error) {
	switch DetectFileType(content) {
	case "zip":
		return readZip(content)
	case "json", "text":
		return string(content), nil
	default:
		return "", fmt.Errorf("unsupported file")
	}
}

func readZip(content []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", err
	}

	var fallback *zip.File
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(file.Name)
		if strings.EqualFold(name, "conversations.json") {
			return readZipEntry(file)
		}
		// 其他的文本
		if fallback == nil && isTextName(name) {
			fallback = file
		}
	}
	if fallback == nil {
		return "", fmt.Errorf("zip 里没有可读取的文本")
	}
	return readZipEntry(fallback)
}

func readZipEntry(file *zip.File) (string, error) {
	rc, err := file.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func isTextName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json", ".txt", ".md", ".html", ".htm":
		return true
	default:
		return false
	}
}

func DetectFileType(content []byte) string {
	if len(content) >= 4 && bytes.Equal(content[:4], []byte{'P', 'K', 3, 4}) {
		return "zip"
	}

	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return "json"
	}

	if len(content) > 0 && utf8.Valid(content) {
		return "text"
	}

	return ""
}

// Parse 按来源把文本拆成一段或多段对话，并填上 user / assistant。
func Parse(raw string, sourceTag store.SourceTag) ([]Conversation, error) {
	switch sourceTag {
	case store.SourceChatGPT:
		return parseChatGPT(raw)
	default:
		messages := splitPlain(raw, sourceTag)
		if len(messages) == 0 {
			return nil, fmt.Errorf("text has no importable content")
		}
		return []Conversation{{Messages: messages}}, nil
	}
}

func splitPlain(raw string, sourceTag store.SourceTag) []store.Message {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	var messages []store.Message
	for _, block := range strings.Split(normalized, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		messages = append(messages, store.Message{
			Seq:       len(messages) + 1,
			Role:      "user",
			SourceTag: sourceTag,
			Content:   block,
		})
	}
	return messages
}

type chatGPTConversation struct {
	Title       string                 `json:"title"`
	CurrentNode string                 `json:"current_node"`
	Mapping     map[string]chatGPTNode `json:"mapping"`
}

type chatGPTNode struct {
	Parent   *string         `json:"parent"`
	Children []string        `json:"children"`
	Message  *chatGPTMessage `json:"message"`
}

type chatGPTMessage struct {
	Author struct {
		Role string `json:"role"`
	} `json:"author"`
	Content struct {
		Parts []json.RawMessage `json:"parts"`
	} `json:"content"`
}

func parseChatGPT(raw string) ([]Conversation, error) {
	raw = strings.TrimSpace(raw)

	var convos []chatGPTConversation
	if err := json.Unmarshal([]byte(raw), &convos); err != nil {
		var one chatGPTConversation
		if errOne := json.Unmarshal([]byte(raw), &one); errOne != nil {
			return nil, fmt.Errorf("parse chatgpt export: %w", err)
		}
		convos = []chatGPTConversation{one}
	}

	parsed := make([]Conversation, 0, len(convos))
	for _, conv := range convos {
		messages := messagesFromChatGPT(conv)
		if len(messages) == 0 {
			continue
		}
		parsed = append(parsed, Conversation{
			Title:    conv.Title,
			Messages: messages,
		})
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("chatgpt export has no messages")
	}
	return parsed, nil
}

func messagesFromChatGPT(conv chatGPTConversation) []store.Message {
	var messages []store.Message
	for _, node := range orderedChatGPTNodes(conv) {
		if node.Message == nil {
			continue
		}
		role := node.Message.Author.Role
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(chatGPTPartText(node.Message.Content.Parts))
		if text == "" {
			continue
		}
		messages = append(messages, store.Message{
			Seq:       len(messages) + 1,
			Role:      role,
			SourceTag: store.SourceChatGPT,
			Content:   text,
		})
	}
	return messages
}

// orderedChatGPTNodes 沿当前分支取出节点。有 current_node 时从叶子往根走再倒序，
// 否则从根沿着最后一个子节点往下走。
func orderedChatGPTNodes(conv chatGPTConversation) []chatGPTNode {
	if conv.CurrentNode != "" {
		return walkChatGPTParents(conv, conv.CurrentNode)
	}
	return walkChatGPTChildren(conv, chatGPTRoot(conv))
}

func chatGPTRoot(conv chatGPTConversation) string {
	for id, node := range conv.Mapping {
		if node.Parent == nil || *node.Parent == "" {
			return id
		}
	}
	return ""
}

func walkChatGPTParents(conv chatGPTConversation, id string) []chatGPTNode {
	var chain []chatGPTNode
	seen := make(map[string]struct{})
	for id != "" {
		if _, ok := seen[id]; ok {
			break
		}
		seen[id] = struct{}{}
		node, ok := conv.Mapping[id]
		if !ok {
			break
		}
		chain = append(chain, node)
		if node.Parent == nil {
			break
		}
		id = *node.Parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

func walkChatGPTChildren(conv chatGPTConversation, id string) []chatGPTNode {
	var chain []chatGPTNode
	seen := make(map[string]struct{})
	for id != "" {
		if _, ok := seen[id]; ok {
			break
		}
		seen[id] = struct{}{}
		node, ok := conv.Mapping[id]
		if !ok {
			break
		}
		chain = append(chain, node)
		if len(node.Children) == 0 {
			break
		}
		id = node.Children[len(node.Children)-1]
	}
	return chain
}

func chatGPTPartText(parts []json.RawMessage) string {
	var builder strings.Builder
	for _, part := range parts {
		var text string
		if err := json.Unmarshal(part, &text); err != nil {
			continue
		}
		if text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(text)
	}
	return builder.String()
}
