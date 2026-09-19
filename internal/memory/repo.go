package memory

import (
	"gorm.io/gorm"
)

type MemoryRepo struct {
	db *gorm.DB
}