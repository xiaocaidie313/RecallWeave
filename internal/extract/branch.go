package extract

import (
	"context"
	"errors"
	"fmt"

	"recallweave/internal/store"
)

// checkGlobalExtraction is shared by full and incremental extraction, including
// imports and the manual HTTP endpoint. Unknown scope fails closed.
func (e *ExtractExcuter) checkGlobalExtraction(ctx context.Context, sessionID uint) error {
	if e.db == nil || sessionID == 0 {
		return errors.New("extract: database and session id are required")
	}
	var scope struct {
		SessionID            uint
		ConversationID       *uint
		FoundConversationID  *uint
		ParentConversationID *uint
	}
	err := e.db.WithContext(ctx).Table("sessions").
		Select("sessions.id AS session_id, sessions.conversation_id, conversations.id AS found_conversation_id, conversations.parent_conversation_id").
		Joins("LEFT JOIN conversations ON conversations.id = sessions.conversation_id").
		Where("sessions.id = ?", sessionID).Take(&scope).Error
	if err != nil {
		return fmt.Errorf("extract: determine session memory scope: %w", err)
	}
	if scope.ConversationID != nil && scope.FoundConversationID == nil {
		return errors.New("extract: conversation memory scope is unavailable")
	}
	if scope.ParentConversationID != nil {
		return store.ErrBranchMemoryPrivate
	}
	return nil
}
