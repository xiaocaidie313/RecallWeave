package run

import (
	"context"
	"errors"

	"recallweave/internal/types"

	"gorm.io/gorm"
)

var ErrConversationNotFound = errors.New("run: conversation not found")

type RunManager struct {
	db *gorm.DB
}

func NewRunManager(db *gorm.DB) *RunManager {
	return &RunManager{db: db}
}

func (r *RunManager) SaveAgentRun(ctx context.Context, run types.AgentRun) error {
	if r == nil || r.db == nil || run.RunID == "" {
		return nil
	}
	return r.db.WithContext(ctx).Create(&run).Error
}

func (r *RunManager) GetAgentRun(ctx context.Context, runID string) (types.AgentRun, error) {
	var run types.AgentRun
	err := r.db.WithContext(ctx).Where("run_id = ?", runID).First(&run).Error
	return run, err
}

func (r *RunManager) ListAgentRuns(ctx context.Context, conversationID uint) ([]types.AgentRun, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&types.AgentRun{}).Where("conversation_id = ?", conversationID).Count(&count).Error
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrConversationNotFound
	}

	var runs []types.AgentRun
	err = r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id DESC").
		Limit(20).
		Find(&runs).Error
	return runs, err
}
