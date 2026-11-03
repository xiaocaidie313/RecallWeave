package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"recallweave/internal/types"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidDeactivation = errors.New("memory: memory_id and reason are required")

// DeactivateMemory 停用一条记忆，保留内容和原文位置。重试不改写首次停用原因和时间。
func (m *MemoryManger) DeactivateMemory(ctx context.Context, memoryID uint, reason string) (types.Memory, error) {
	reason = strings.TrimSpace(reason)
	if memoryID == 0 || reason == "" {
		return types.Memory{}, ErrInvalidDeactivation
	}
	var item types.Memory
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", memoryID).Take(&item).Error; err != nil {
			return err
		}
		if item.Status == types.MemoryInactive {
			return nil
		}
		if item.Status != types.MemoryActive {
			return fmt.Errorf("memory: cannot deactivate status %q", item.Status)
		}
		now := tx.NowFunc()
		result := tx.Model(&types.Memory{}).Where("id = ? AND status = ?", memoryID, types.MemoryActive).
			Updates(map[string]any{
				"status":              types.MemoryInactive,
				"deactivated_at":      now,
				"deactivation_reason": reason,
				"updated_at":          now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("memory: state changed during deactivation")
		}
		item.Status = types.MemoryInactive
		item.DeactivatedAt = &now
		item.DeactivationReason = reason
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return types.Memory{}, err
	}
	return item, nil
}
