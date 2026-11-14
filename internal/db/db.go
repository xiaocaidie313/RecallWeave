package db

import (
	"fmt"
	"recallweave/internal/config"
	"recallweave/internal/types"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 目前只做postgres 的 连接
func NewDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		cfg.UserName,
		cfg.Password,
		cfg.Host,
		cfg.Port,
		cfg.DBName,
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return db, nil
}

func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&types.Conversation{},
		&types.Session{},
		&types.TranscriptSource{},
		&types.Message{},
		&types.Memory{},
		&types.AgentRun{},
		&types.SubagentSession{},
		&types.SubagentTask{},
		&types.TaskGroup{},
		&types.ConversationEvent{},
	); err != nil {
		return err
	}
	// 导入会话的 conversation_id 需要能留空。conversation_id 可以为空
	if err := db.Migrator().AlterColumn(&types.Session{}, "ConversationID"); err != nil {
		return err
	}
	return ensureMemoryEmbeddingIndex(db)
}

// ensureEmbeddingIndex 把向量列收成可建 HNSW 的宽度，再按余弦距离建索引。
// 查询用的是 <=>，运算符类必须是 vector_cosine_ops。
func ensureMemoryEmbeddingIndex(db *gorm.DB) error {
	if !db.Migrator().HasTable(&types.Memory{}) {
		return nil
	}

	var width int
	// 获取 memory 的 embedding 的宽度 多少维
	if err := db.Raw(`SELECT atttypmod FROM pg_attribute WHERE attrelid = 'memories'::regclass AND attname = 'embedding'`).Scan(&width).Error; err != nil {
		return err
	}
	if width != types.EmbeddingDim {
		var typesd int64 // 统计 非空的emmbedding 的 数量
		if err := db.Model(&types.Memory{}).Where("embedding IS NOT NULL").Count(&typesd).Error; err != nil {
			return err
		}
		if typesd > 0 { // 有非空的emmbedding 需要重新嵌入
			return fmt.Errorf("db: %d memories use vector(%d); re-embed them before narrowing to vector(%d)", typesd, width, types.EmbeddingDim)
		}
		// 执行sql 修改memory 的 embedding 的宽度 多少维
		if err := db.Exec(fmt.Sprintf(`ALTER TABLE memories ALTER COLUMN embedding TYPE vector(%d)`, types.EmbeddingDim)).Error; err != nil {
			return err
		}
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_embedding_hnsw ON memories USING hnsw (embedding vector_cosine_ops)`).Error
}
