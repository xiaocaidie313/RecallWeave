package retrieval

import (
	"context"
	"testing"

	"recallweave/internal/store"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type visibilityEmbedder struct{}

func (visibilityEmbedder) Embed(context.Context, string) ([]float64, error) {
	values := make([]float64, store.EmbeddingDim)
	values[0] = 1
	return values, nil
}

func TestVectorSearchFiltersBeforeLimit(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	retriever := New(db, visibilityEmbedder{})
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE embedding IS NOT NULL AND status = \$1 ORDER BY embedding <=> \$2 LIMIT \$3`).
		WithArgs(store.MemoryActive, sqlmock.AnyArg(), 5).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := retriever.Search(context.Background(), "方案", "", 5); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .* FROM "memories" WHERE embedding IS NOT NULL ORDER BY embedding <=> \$1 LIMIT \$2`).
		WithArgs(sqlmock.AnyArg(), 5).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := retriever.Search(context.Background(), "方案", "", 5, true); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
