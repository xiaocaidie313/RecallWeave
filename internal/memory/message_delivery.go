package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"recallweave/internal/store"
)

// MessageDelivery is the provenance already committed with a message.created
// event. Sources keep the event's wire format, including source_run_id.
type MessageDelivery struct {
	TaskID    string          `json:"task_id,omitempty"`
	GroupID   string          `json:"group_id,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	TaskRunID string          `json:"task_run_id,omitempty"`
	Sources   json.RawMessage `json:"sources,omitempty"`
}

// MessageDeliveries restores old and new deliveries from the durable event log.
// The event key is indexed with conversation_id, so this needs no replay scan
// or new message columns. The caller supplies the session it has already loaded.
func (m *MemoryManger) MessageDeliveries(ctx context.Context, session store.Session, messages []store.Message) (map[uint]MessageDelivery, error) {
	deliveries := make(map[uint]MessageDelivery) // 存储交付
	if ctx == nil || m == nil || m.db == nil || session.ID == 0 {
		return nil, fmt.Errorf("memory: message delivery session and storage are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if session.ConversationID == nil || *session.ConversationID == 0 {
		return deliveries, nil
	}
	owner := *session.ConversationID
	byKey := make(map[string]store.Message, len(messages)) // 标记键切片+存储消息
	keys := make([]string, 0, len(messages))               // 键切片+存储键
	for _, message := range messages {
		if message.ID == 0 || message.SessionID != session.ID {
			continue
		}
		key := fmt.Sprintf("message:%d", message.ID)
		if _, exists := byKey[key]; !exists {
			keys = append(keys, key)
			byKey[key] = message
		}
	}
	// Bound SQL parameters even when the history endpoint returns a long session.
	for offset := 0; offset < len(keys); offset += 256 {
		end := offset + 256
		if end > len(keys) {
			end = len(keys)
		}
		var events []store.ConversationEvent
		if err := m.db.WithContext(ctx).Select("conversation_id", "kind", "key", "data").
			Where("conversation_id = ? AND kind = ? AND key IN ?", owner, "message.created", keys[offset:end]).
			Find(&events).Error; err != nil {
			return nil, err
		}
		for _, event := range events {
			message, exists := byKey[event.Key]
			if !exists || event.ConversationID != owner || event.Kind != "message.created" {
				continue
			}
			var payload struct {
				ConversationID uint `json:"conversation_id"`
				MessageDelivery
				Message store.Message `json:"message"`
			}
			if json.Unmarshal(event.Data, &payload) != nil || payload.ConversationID != owner ||
				payload.Message.ID != message.ID || payload.Message.SessionID != session.ID ||
				payload.Message.Seq != message.Seq || payload.Message.Role != message.Role ||
				payload.Message.Content != message.Content || !validDeliverySources(payload.Sources) {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(payload.Sources), []byte("null")) {
				payload.Sources = nil
			}
			deliveries[message.ID] = payload.MessageDelivery
		}
	}
	return deliveries, nil
}

func validDeliverySources(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	var sources []map[string]json.RawMessage
	if json.Unmarshal(raw, &sources) != nil {
		return false
	}
	for _, source := range sources {
		if source == nil {
			return false
		}
	}
	return true
}
