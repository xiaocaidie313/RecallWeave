package thinking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"recallweave/internal/types"

	"gorm.io/gorm"
)

var ErrInvalidCheckpoint = errors.New("thinking: invalid checkpoint")

const MaxCheckpointTextRunes = 2000

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// Save replaces the current checkpoint only on an explicit user write. Parent row
// locking serializes saves with deletion; the primary key limits each conversation
// to one checkpoint. Retrying identical content preserves its modification time.
func (s *Store) Save(ctx context.Context, conversationID uint, text, direction string) (*types.ThinkingCheckpoint, error) {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') || utf8.RuneCountInString(text) > MaxCheckpointTextRunes {
		return nil, fmt.Errorf("%w: text must contain 1 to %d characters of nonblank UTF-8 text without NUL", ErrInvalidCheckpoint, MaxCheckpointTextRunes)
	}
	switch direction {
	case "", "explore", "reject", "reframe":
	default:
		return nil, fmt.Errorf("%w: direction must be explore, reject, reframe or empty", ErrInvalidCheckpoint)
	}
	var checkpoint types.ThinkingCheckpoint
	result := s.db.WithContext(ctx).Raw(`
		INSERT INTO thinking_checkpoints AS prior (conversation_id, text, direction, author, updated_at)
		SELECT id, ?, ?, 'user', statement_timestamp() FROM conversations WHERE id = ? FOR KEY SHARE
		ON CONFLICT (conversation_id) DO UPDATE SET text = EXCLUDED.text, direction = EXCLUDED.direction,
			updated_at = CASE WHEN prior.text = EXCLUDED.text AND prior.direction = EXCLUDED.direction
				THEN prior.updated_at ELSE clock_timestamp() END
		RETURNING conversation_id, text, direction, author, updated_at`, text, direction, conversationID).Scan(&checkpoint)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &checkpoint, nil
}

// Latest distinguishes an existing conversation with no checkpoint (nil, nil)
// from an unknown conversation (gorm.ErrRecordNotFound) in one snapshot.
func (s *Store) Latest(ctx context.Context, conversationID uint) (*types.ThinkingCheckpoint, error) {
	var checkpoint types.ThinkingCheckpoint
	result := s.db.WithContext(ctx).Table("conversations AS c").Select("tc.*").
		Joins("LEFT JOIN thinking_checkpoints AS tc ON tc.conversation_id = c.id").
		Where("c.id = ?", conversationID).Scan(&checkpoint)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	if checkpoint.ConversationID == 0 {
		return nil, nil
	}
	return &checkpoint, nil
}
