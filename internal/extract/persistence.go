package extract

import (
	"context"

	"recallweave/internal/types"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 包括停用记忆的覆盖范围，避免增量提炼把已停用的原文重新写成活跃记忆。
func coveredSeq(db *gorm.DB, sessionID uint) (int, error) {
	var row struct{ Seq int }
	err := db.Model(&types.Memory{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq_end), 0) AS seq").Scan(&row).Error
	return row.Seq, err
}

// Session 行始终存在，即使还没有任何记忆也能串行化提交。
// 此事务只锁 Session 和 Memory，不再获取 Conversation 锁。
func lockExtractionSession(tx *gorm.DB, sessionID uint) error {
	var session types.Session
	return tx.Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", sessionID).Take(&session).Error
}

// false 表示生成摘要期间覆盖范围已变化，调用方必须重读原文并重新生成。
func (e *ExtractExcuter) appendMemories(ctx context.Context, sessionID uint, expectedCovered int, memories []types.Memory) (bool, error) {
	appended := false
	err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockExtractionSession(tx, sessionID); err != nil {
			return err
		}
		current, err := coveredSeq(tx, sessionID)
		if err != nil {
			return err
		}
		if current != expectedCovered {
			return nil
		}
		if len(memories) > 0 {
			if err := tx.Create(&memories).Error; err != nil {
				return err
			}
		}
		appended = true
		return nil
	})
	return appended && err == nil, err
}

// 全量重写也使用同一 Session 行锁，避免删除其他执行器刚提炼的新消息。
func (e *ExtractExcuter) replaceMemories(ctx context.Context, sessionID uint, snapshotEnd int, memories []types.Memory) (bool, error) {
	replaced := false
	err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockExtractionSession(tx, sessionID); err != nil {
			return err
		}
		var existing []types.Memory
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id = ?", sessionID).Order("id").Find(&existing).Error; err != nil {
			return err
		}
		covered := 0
		for _, item := range existing {
			if item.Status != "" && item.Status != types.MemoryActive {
				return types.ErrMemoryHistoryProtected
			}
			if item.SeqEnd > covered {
				covered = item.SeqEnd
			}
		}
		if covered > snapshotEnd {
			return nil
		}
		if err := tx.Where("session_id = ?", sessionID).Delete(&types.Memory{}).Error; err != nil {
			return err
		}
		if len(memories) > 0 {
			if err := tx.Create(&memories).Error; err != nil {
				return err
			}
		}
		replaced = true
		return nil
	})
	return replaced && err == nil, err
}
