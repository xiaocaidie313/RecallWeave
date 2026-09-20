package ingest

import (
	"recallweave/internal/store"

	"gorm.io/gorm"
)

type IngestRepo struct {
	db *gorm.DB
}

func NewIngestRepo(db *gorm.DB) *IngestRepo {
	return &IngestRepo{db: db}
}

// CreateSourceWithMessages 在一个事务里写入来源和它的消息。
// source 插入后自增 ID 才可用，所以 SourceID 在这里回填。
func (r *IngestRepo) CreateSourceWithMessages(source *store.Source, messages []store.Message) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(source).Error; err != nil {
			return err
		}

		if len(messages) == 0 {
			return nil
		}

		for i := range messages {
			messages[i].SourceID = source.ID
		}

		return tx.Create(&messages).Error
	})
}
