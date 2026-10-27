package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

const MaxForkMessages = 200
const MaxForkSnapshotBytes = 256 << 10

var ErrInvalidFork = errors.New("invalid fork point or title")
var ErrForkTooLarge = errors.New("fork snapshot exceeds 200 messages or 256 KiB")

type ForkResult struct {
	Conversation   store.Conversation `json:"conversation"`
	SessionID      uint               `json:"session_id"`
	InheritedCount int                `json:"inherited_count"`
}

func (m *MemoryManger) GetConversation(ctx context.Context, id uint) (store.Conversation, error) {
	var conversation store.Conversation
	if err := m.db.WithContext(ctx).Model(&store.Conversation{}).Where("id = ?", id).First(&conversation).Error; err != nil {
		return store.Conversation{}, err
	}
	return conversation, nil
}

// ForkConversation creates an independent empty chat and a frozen context
// prefix. Original messages are not copied into the new session or re-extracted.
func (m *MemoryManger) ForkConversation(ctx context.Context, parentID uint, fromSeq int, title string) (ForkResult, error) {
	title = strings.TrimSpace(title)
	if parentID == 0 || fromSeq <= 0 || utf8.RuneCountInString(title) > 191 {
		return ForkResult{}, ErrInvalidFork
	}
	var result ForkResult
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var parent store.Conversation
		if err := tx.First(&parent, parentID).Error; err != nil {
			return err
		}
		// 找session 
		var source store.Session
		if err := tx.Where("conversation_id = ? AND source_tag = ?", parentID, store.SourceChat).First(&source).Error; err != nil {
			return err
		}
		var messages []store.Message
		// Bound the read before constructing or writing a potentially large snapshot.
		// 找message
		if err := tx.Where("session_id = ? AND seq <= ?", source.ID, fromSeq).
			Order("seq").Limit(MaxForkMessages + 1).Find(&messages).Error; err != nil {
			return err
		}
		if len(messages) > MaxForkMessages {
			return ErrForkTooLarge
		}
		if len(messages) == 0 || messages[len(messages)-1].Seq != fromSeq {
			return fmt.Errorf("%w: message %d does not exist", ErrInvalidFork, fromSeq)
		}

		inherited, err := parent.InheritedMessages()
		if err != nil {
			return fmt.Errorf("invalid parent snapshot: %w", err)
		}
		for _, message := range messages {
			if message.Role != "user" && message.Role != "assistant" {
				return fmt.Errorf("%w: unsupported message role", ErrInvalidFork)
			}
			inherited = append(inherited, store.ContextMessage{
				SessionID: source.ID, Seq: message.Seq, Role: message.Role, Content: message.Content,
			})
		}
		if len(inherited) > MaxForkMessages {
			return ErrForkTooLarge
		}
		snapshot, err := json.Marshal(inherited)
		if err != nil {
			return err
		}
		if len(snapshot) > MaxForkSnapshotBytes {
			return ErrForkTooLarge
		}
		if title == "" {
			title = "分支对话"
		}
		// 创建新对话
		child := store.Conversation{Title: title, ParentConversationID: &parentID,
			ForkSessionID: &source.ID, ForkSeq: fromSeq, ForkSnapshot: snapshot}
		if err := tx.Create(&child).Error; err != nil {
			return err
		}
		// 创建新session
		session := store.Session{ConversationID: &child.ID, SourceTag: store.SourceChat,
			Name: "chat", ExtractStatus: store.ExtractSkipped}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		result = ForkResult{Conversation: child, SessionID: session.ID, InheritedCount: len(inherited)}
		return nil
	})
	if err != nil {
		return ForkResult{}, err
	}
	return result, nil
}
