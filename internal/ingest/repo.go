package ingest

import (
	"context"

	"recallweave/internal/store"

	"gorm.io/gorm"
)

type IngestRepo struct {
	db *gorm.DB
}

func NewIngestRepo(db *gorm.DB) *IngestRepo {
	return &IngestRepo{db: db}
}

// CreateSessionWithMessages 在一个事务里写入会话和它的消息。
// session 插入后自增 ID 才可用，所以 SessionID 在这里回填。
func (r *IngestRepo) CreateSessionWithMessages(ctx context.Context, session *store.Session, messages []store.Message) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(session).Error; err != nil {
			return err
		}

		if len(messages) == 0 {
			return nil
		}

		for i := range messages {
			messages[i].SessionID = session.ID
		}

		return tx.Create(&messages).Error
	})
}
