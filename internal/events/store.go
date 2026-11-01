package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"recallweave/internal/store"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidCursor = errors.New("events: cursor does not belong to this conversation")
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// LockConversation must precede task/session locks and event ID allocation.
func LockConversation(tx *gorm.DB, owner uint) error {
	if tx == nil || owner == 0 {
		return fmt.Errorf("events: transaction and conversation are required")
	}
	var conversation store.Conversation
	// 单行锁住  id 为 owner 的 conversation
	return tx.Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", owner).Take(&conversation).Error
}

// Append participates in the caller's transaction, whose conversation lock is held.
func Append(tx *gorm.DB, event *store.ConversationEvent) error {
	if tx == nil || event == nil || event.ConversationID == 0 || event.ID != 0 || !kindPattern.MatchString(event.Kind) || strings.TrimSpace(event.Key) == "" || len(event.Key) > 191 || !utf8.ValidString(event.Key) || len(event.Data) == 0 || !utf8.Valid(event.Data) || !json.Valid(event.Data) {
		return fmt.Errorf("events: invalid event")
	}
	record := *event
	if err := tx.Create(&record).Error; err != nil {
		return err
	}
	*event = record
	return nil
}

func (s *Store) check(ctx context.Context, owner uint) error {
	if ctx == nil || s == nil || s.db == nil || owner == 0 {
		return fmt.Errorf("events: context, storage and conversation are required")
	}
	return ctx.Err()
}

func (s *Store) ConversationExists(ctx context.Context, owner uint) (bool, error) {
	if err := s.check(ctx, owner); err != nil {
		return false, err
	}
	var count int64
	err := s.db.WithContext(ctx).Model(&store.Conversation{}).Where("id = ?", owner).Count(&count).Error
	return count != 0, err
}

func (s *Store) ValidateCursor(ctx context.Context, owner uint, after uint64) error {
	if err := s.check(ctx, owner); err != nil {
		return err
	}
	if after == 0 {
		return nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&store.ConversationEvent{}).Where("id = ? AND conversation_id = ?", after, owner).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return ErrInvalidCursor
	}
	return nil
}

func (s *Store) List(ctx context.Context, owner uint, after uint64, limit int) ([]store.ConversationEvent, error) {
	if err := s.check(ctx, owner); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows := make([]store.ConversationEvent, 0)
	err := s.db.WithContext(ctx).Where("conversation_id = ? AND id > ?", owner, after).Order("id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}
