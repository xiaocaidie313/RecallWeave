package db

import (
	"fmt"

	"recallweave/internal/config"
	"recallweave/internal/store"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func NewDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.UserName,
		cfg.Password,
		cfg.Host,
		cfg.Port,
		cfg.DBName,
	)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return db, nil
}

func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&store.Conversation{},
		&store.Session{},
		&store.Message{},
		&store.Memory{},
	); err != nil {
		return err
	}

	// AutoMigrate 不会改已有列的非空约束，也不会删掉结构体里已经去掉的字段。
	// messages_ids 是早期误加的外键列，消息已经用 session_id 关联。
	if db.Migrator().HasColumn(&store.Session{}, "messages_ids") {
		if err := db.Migrator().DropColumn(&store.Session{}, "messages_ids"); err != nil {
			return err
		}
	}
	// 导入会话的 conversation_id 需要能留空。已有的非空列要改成可空。
	return db.Migrator().AlterColumn(&store.Session{}, "ConversationID")
}
