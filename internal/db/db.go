package db

import (
	"fmt"

	"recallweave/internal/config"
	"recallweave/internal/store"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func NewDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		cfg.UserName,
		cfg.Password,
		cfg.Host,
		cfg.Port,
		cfg.DBName,
	)
	// db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
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
	if err := db.Migrator().AlterColumn(&store.Session{}, "ConversationID"); err != nil {
		return err
	}
	// attach 曾用来区分 context / continue，绑定本身就是 context，这一列去掉。
	if db.Migrator().HasColumn(&store.Session{}, "attach") {
		if err := db.Migrator().DropColumn(&store.Session{}, "attach"); err != nil {
			return err
		}
	}
	return ensureEmbeddingIndex(db)
}

// ensureEmbeddingIndex 把向量列收成可建 HNSW 的宽度，再按余弦距离建索引。
// 查询用的是 <=>，运算符类必须是 vector_cosine_ops。
func ensureEmbeddingIndex(db *gorm.DB) error {
	if !db.Migrator().HasTable(&store.Memory{}) {
		return nil
	}

	var width int
	if err := db.Raw(`SELECT atttypmod FROM pg_attribute WHERE attrelid = 'memories'::regclass AND attname = 'embedding'`).Scan(&width).Error; err != nil {
		return err
	}
	if width != store.EmbeddingDim {
		var stored int64
		if err := db.Model(&store.Memory{}).Where("embedding IS NOT NULL").Count(&stored).Error; err != nil {
			return err
		}
		if stored > 0 {
			return fmt.Errorf("db: %d memories use vector(%d); re-embed them before narrowing to vector(%d)", stored, width, store.EmbeddingDim)
		}
		if err := db.Exec(fmt.Sprintf(`ALTER TABLE memories ALTER COLUMN embedding TYPE vector(%d)`, store.EmbeddingDim)).Error; err != nil {
			return err
		}
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_embedding_hnsw ON memories USING hnsw (embedding vector_cosine_ops)`).Error
}
