package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

var ErrSessionConflict = errors.New("subagent: session changed during execution")
var ErrCheckpointUnavailable = errors.New("subagent: newer work could not save its context; this checkpoint cannot be continued")

// SessionStore persists one child conversation without granting access to
// another parent conversation's work. Update uses the loaded revision as CAS.
type SubagentSessionStore interface {
	Create(context.Context, *store.SubagentSession) error
	Load(context.Context, string, uint) (store.SubagentSession, error)
	Update(context.Context, *store.SubagentSession, uint64) error
	List(context.Context, uint, int) ([]store.SubagentSession, error)
}

// CheckpointVerifier is optional for repositories that can inspect durable Run
// records. It prevents a restart from silently restoring a stale checkpoint
// after a newer execution failed to persist its context.
type CheckpointVerifier interface {
	CheckCheckpoint(context.Context, store.SubagentSession) error
}

type GormSubagentSessionStore struct {
	db *gorm.DB
}

func NewSubagentSessionStore(db *gorm.DB) *GormSubagentSessionStore {
	return &GormSubagentSessionStore{db: db}
}

var _ SubagentSessionStore = (*GormSubagentSessionStore)(nil)
var _ CheckpointVerifier = (*GormSubagentSessionStore)(nil)

func (s *GormSubagentSessionStore) check(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("subagent: storage context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil {
		return fmt.Errorf("subagent: session storage is unavailable")
	}

	return nil
}

func checkSessionIdentity(id string, conversationID uint) error {
	if id == "" || len(id) > 64 || strings.TrimSpace(id) != id || conversationID == 0 {
		return fmt.Errorf("subagent: session id and owning conversation are required")
	}
	return nil
}

// 单纯检查 json 是否合法
func checkSessionJSON(session *store.SubagentSession) error {
	for _, value := range []json.RawMessage{session.Snapshot, session.Preset, session.Sources} {
		if len(value) > 0 && !json.Valid(value) {
			return fmt.Errorf("subagent: stored context must be valid JSON")
		}
	}
	return nil
}

func (s *GormSubagentSessionStore) Create(ctx context.Context, session *store.SubagentSession) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("subagent: session is required")
	}
	if err := checkSessionIdentity(session.ID, session.ConversationID); err != nil {
		return err
	}
	if err := checkSessionJSON(session); err != nil {
		return err
	}
	record := *session
	if record.Revision == 0 {
		record.Revision = 1
	}
	// Create deliberately has no upsert: an existing identity must never be
	// overwritten by a newly delegated task.
	if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
		return err
	}
	*session = record
	return nil
}

func (s *GormSubagentSessionStore) Load(ctx context.Context, id string, conversationID uint) (store.SubagentSession, error) {
	var session store.SubagentSession
	if err := s.check(ctx); err != nil {
		return session, err
	}
	if err := checkSessionIdentity(id, conversationID); err != nil {
		return session, err
	}

	if err := s.db.WithContext(ctx).Where("id = ? AND conversation_id = ?", id, conversationID).Take(&session).Error; err != nil {
		return session, err
	}
	return session, nil
}

func (s *GormSubagentSessionStore) Update(ctx context.Context, session *store.SubagentSession, expectedRevision uint64) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("subagent: session is required")
	}
	if err := checkSessionIdentity(session.ID, session.ConversationID); err != nil {
		return err
	}
	if expectedRevision == 0 || expectedRevision == math.MaxUint64 {
		return fmt.Errorf("subagent: a valid loaded revision is required")
	}
	if err := checkSessionJSON(session); err != nil {
		return err
	}
	// 增加副本
	record := *session
	// 版本号 +1 用于乐观锁 防止两个请求同时更新同一个会话
	// 跟新一次 在原来的id 和 conversation_id 和 版本号 都相同的情况下  改版本号  expectedRevision 是旧版本
	record.Revision = expectedRevision + 1  
	record.UpdatedAt = s.db.NowFunc()
	result := s.db.WithContext(ctx).Model(&store.SubagentSession{}).
		Where("id = ? AND conversation_id = ? AND revision = ?", record.ID, record.ConversationID, expectedRevision).
		Select("agent_name", "task", "latest_run_id", "status", "snapshot", "preset", "sources", "revision", "updated_at").
		Updates(&record)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrSessionConflict
	}
	*session = record
	return nil
}

// 同一对话下 列出所有子agent session
func (s *GormSubagentSessionStore) List(ctx context.Context, conversationID uint, limit int) ([]store.SubagentSession, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if conversationID == 0 {
		return nil, fmt.Errorf("subagent: owning conversation is required")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	sessions := make([]store.SubagentSession, 0)

	err := s.db.WithContext(ctx).Where("conversation_id = ?", conversationID).Order("updated_at DESC").Order("id DESC").Limit(limit).Find(&sessions).Error
	return sessions, err
}

func (s *GormSubagentSessionStore) CheckCheckpoint(ctx context.Context, session store.SubagentSession) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := checkSessionIdentity(session.ID, session.ConversationID); err != nil {
		return err
	}
	if session.LatestRunID == "" || len(session.LatestRunID) > 64 || strings.TrimSpace(session.LatestRunID) != session.LatestRunID {
		return fmt.Errorf("subagent: checkpoint run id is required")
	}
	var failed int64
	err := s.db.WithContext(ctx).Model(&store.AgentRun{}).
		Where("subagent_id = ? AND conversation_id = ? AND previous_run_id = ? AND run_id <> ?",
			session.ID, session.ConversationID, session.LatestRunID, session.LatestRunID).
		Where("COALESCE(state->>'context_persist_failed', 'false') = 'true'").
		Count(&failed).Error
	if err != nil {
		return err
	}
	if failed > 0 {
		return ErrCheckpointUnavailable
	}
	return nil
}
