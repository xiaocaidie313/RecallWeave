package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	openai "github.com/openai/openai-go/v3"
)

func snapshotMessages(t *testing.T, value json.RawMessage) []map[string]any {
	t.Helper()
	var messages []map[string]any
	if err := json.Unmarshal(value, &messages); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestConversationSnapshotPreservesActualSDKToolExchange(t *testing.T) {
	conversation := NewConversation("OLD_SYSTEM_POLICY", WithConversationID(17))
	conversation.AddUserMessage("查一下最初的决定")
	// Use precisely the real Client.Next conversion, including SDK response
	// union decoding, rather than reconstructing calls from RunStep summaries.
	var response openai.ChatCompletionMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"开始核查","tool_calls":[{"id":"call_original_1","type":"function","function":{"name":"get_messages","arguments":"{\"session_id\":8,\"seq_start\":1,\"seq_end\":1}"}},{"id":"call_original_2","type":"function","function":{"name":"mcp_tavily_extract","arguments":"{\"urls\":[\"https://example.test/docs\"]}"}}]}`), &response); err != nil {
		t.Fatal(err)
	}
	conversation.messages = append(conversation.messages, response.ToParam())
	conversation.AddToolResult("call_original_2", `{"results":[{"url":"https://example.test/docs","raw_content":"actual page"}]}`)
	conversation.AddToolResult("call_original_1", `{"session_id":8,"messages":[{"seq":1,"content":"最初使用 Go"}]}`)
	conversation.AddAssistantMessage("已核对，最初使用 Go。")
	conversation.AddSystemMessage("此前运行计划已结束。")

	snapshot, err := conversation.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var stored ConversationSnapshot
	if err := json.Unmarshal(serialized, &stored); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreConversation("CURRENT_SYSTEM_POLICY", &stored, WithConversationID(17))
	if err != nil {
		t.Fatal(err)
	}
	restored.AddUserMessage("再核对后来是否改了方案")

	// The next real SDK request must contain the original matched tool calls
	// and replies. Restoring has no executor and cannot replay these tools.
	payload, err := json.Marshal(openai.ChatCompletionNewParams{Model: "test-model", Messages: restored.messages})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	want := snapshotMessages(t, snapshot.Messages)
	want[0]["content"] = "CURRENT_SYSTEM_POLICY"
	want = append(want, map[string]any{"role": "user", "content": "再核对后来是否改了方案"})
	if got := snapshotMessages(t, request.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("SDK request lost original history:\ngot=%s\nwant=%+v", request.Messages, want)
	}
	if restored.ID != 17 || strings.Contains(string(payload), "OLD_SYSTEM_POLICY") {
		t.Fatal("conversation association or current policy was lost")
	}

	beforeMutation := string(payload)
	for i := range stored.Messages {
		stored.Messages[i] = ' '
	}
	afterMutation, err := json.Marshal(openai.ChatCompletionNewParams{Model: "test-model", Messages: restored.messages})
	if err != nil || string(afterMutation) != beforeMutation {
		t.Fatal("restored SDK messages borrow mutable snapshot bytes")
	}
}

func TestAddAssistantTurnSupportsCustomGenerators(t *testing.T) {
	conversation := NewConversation("policy")
	conversation.AddUserMessage("read")
	conversation.AddAssistantTurn(Turn{Content: "reading", ToolCalls: []ToolCall{{ID: "read-1", Name: "get_messages", Arguments: `{"session_id":8}`}}})
	conversation.AddToolResult("read-1", `{"messages":[]}`)
	conversation.AddAssistantTurn(Turn{Content: "没有记录"})
	snapshot, err := conversation.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	messages := snapshotMessages(t, snapshot.Messages)
	call := messages[2]["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "read-1" || call["type"] != "function" || messages[3]["tool_call_id"] != "read-1" || messages[4]["content"] != "没有记录" {
		t.Fatalf("custom generator history incorrect: %s", snapshot.Messages)
	}
}

func TestConversationSnapshotAllowsPendingUserAfterModelFailure(t *testing.T) {
	conversation := NewConversation("policy")
	conversation.AddUserMessage("模型失败前已提交的任务")
	snapshot, err := conversation.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreConversation("new policy", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restored.AddUserMessage("按已有信息继续")
	if _, err := restored.Snapshot(); err != nil {
		t.Fatal(err)
	}
}

func TestConversationSnapshotPreservesAssistantRefusalAndName(t *testing.T) {
	snapshot := &ConversationSnapshot{Version: 1, Messages: json.RawMessage(`[{"role":"system","content":"policy"},{"role":"user","content":"question"},{"role":"assistant","name":"reviewer","refusal":"Cannot perform that operation"}]`)}
	restored, err := RestoreConversation("new policy", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := restored.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	messages := snapshotMessages(t, copy.Messages)
	if messages[2]["name"] != "reviewer" || messages[2]["refusal"] != "Cannot perform that operation" {
		t.Fatalf("assistant metadata lost: %s", copy.Messages)
	}
}

func TestConversationSnapshotRejectsInvalidHistory(t *testing.T) {
	for _, test := range []struct{ name, messages string }{
		{"empty", `[]`},
		{"null", `null`},
		{"object", `{"role":"system","content":"policy"}`},
		{"bad_json", `[`},
		{"trailing_json", `[{"role":"system","content":"p"}] {}`},
		{"no_system", `[{"role":"user","content":"p"}]`},
		{"null_message", `[null]`},
		{"unknown_role", `[{"role":"system","content":"p"},{"role":"developer","content":"escape"}]`},
		{"unknown_field", `[{"role":"system","content":"p","api_key":"x"}]`},
		{"role_specific_field", `[{"role":"system","content":"p","tool_call_id":""}]`},
		{"duplicate_role", `[{"role":"system","role":"user","content":"p"}]`},
		{"wrong_content", `[{"role":"system","content":{}}]`},
		{"null_content", `[{"role":"system","content":null}]`},
		{"null_name", `[{"role":"system","content":"p","name":null}]`},
		{"empty_assistant", `[{"role":"system","content":"p"},{"role":"assistant"}]`},
		{"orphan_result", `[{"role":"system","content":"p"},{"role":"tool","tool_call_id":"missing","content":"result"}]`},
		{"unfinished_call", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]}]`},
		{"wrong_result", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"b","content":"x"}]`},
		{"duplicate_call", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]}]`},
		{"duplicate_result", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"x"},{"role":"tool","tool_call_id":"a","content":"x"}]`},
		{"interrupted_batch", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"user","content":"continue"}]`},
		{"missing_arguments", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read"}}]}]`},
		{"custom_call", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"custom","function":{"name":"read","arguments":"{}"}}]}]`},
		{"unknown_nested_field", `[{"role":"system","content":"p"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}","extra":true}}]}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RestoreConversation("new policy", &ConversationSnapshot{Version: 1, Messages: json.RawMessage(test.messages)}); err == nil {
				t.Fatal("invalid history accepted")
			}
		})
	}
	if _, err := RestoreConversation("policy", nil); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	if _, err := RestoreConversation("policy", &ConversationSnapshot{Version: 2, Messages: json.RawMessage(`[{"role":"system","content":"p"}]`)}); err == nil {
		t.Fatal("unknown version accepted")
	}
	if _, err := RestoreConversation(string([]byte{0xff}), &ConversationSnapshot{Version: 1, Messages: json.RawMessage(`[{"role":"system","content":"p"}]`)}); err == nil {
		t.Fatal("invalid system UTF-8 accepted")
	}
}

func TestConversationSnapshotEnforcesSizeAndCountLimits(t *testing.T) {
	conversation := NewConversation("policy")
	for i := 1; i < MaxConversationSnapshotMessages; i++ {
		conversation.AddUserMessage("x")
	}
	if _, err := conversation.Snapshot(); err != nil {
		t.Fatalf("exact message limit rejected: %v", err)
	}
	conversation.AddUserMessage("one too many")
	if _, err := conversation.Snapshot(); err == nil {
		t.Fatal("message overflow accepted")
	}

	large := NewConversation("policy")
	large.AddUserMessage(strings.Repeat("x", MaxConversationSnapshotBytes))
	if _, err := large.Snapshot(); err == nil {
		t.Fatal("snapshot over 512 KiB accepted")
	}
	valid, err := NewConversation("small").Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreConversation(strings.Repeat("y", MaxConversationSnapshotBytes), valid); err == nil {
		t.Fatal("replacement policy bypassed snapshot size limit")
	}
	invalid := &ConversationSnapshot{Version: 1, Messages: json.RawMessage([]byte{'[', '"', 0xff, '"', ']'})}
	if _, err := RestoreConversation("policy", invalid); err == nil {
		t.Fatal("invalid message UTF-8 accepted")
	}
}
