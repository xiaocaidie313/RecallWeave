package chat

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDeliveryQueueSeparatesModelWorkBeforeApplyingBatchLimit(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		for _, synthesize := range []bool{false, true} {
			name := "tasks/direct"
			if grouped {
				name = "groups/combined"
			}
			if synthesize {
				name += "/synthesize"
			}
			t.Run(name, func(t *testing.T) {
				repo, mock := reportTestDB(t)
				table := "subagent_tasks"
				if grouped {
					table = "task_groups"
				}
				pattern := `SELECT "id","conversation_id","delivery_mode" FROM "` + table + `" WHERE .*report_status = .* AND status IN .* AND `
				if synthesize {
					pattern += `delivery_mode = .*`
				} else {
					pattern += `COALESCE\(delivery_mode, ''\) <> .*`
				}
				args := []driver.Value{"pending", "completed", "partial", "failed", "cancelled", "interrupted", "synthesize"}
				if grouped {
					pattern += ` AND COALESCE\(delivery_mode, ''\) <> .*`
					args = append(args, "direct")
				}
				pattern += ` AND conversation_id NOT IN .* ORDER BY finished_at ASC,id ASC LIMIT`
				args = append(args, uint(7), uint(8), 3)
				mode := "direct"
				if grouped {
					mode = "combined"
				}
				if synthesize {
					mode = "synthesize"
				}
				mock.ExpectQuery(pattern).WithArgs(args...).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "delivery_mode"}).AddRow("ready", uint(9), mode))
				if grouped {
					rows, err := repo.PendingGroupDelivery(context.Background(), 3, synthesize, []uint{7, 8})
					if err != nil || len(rows) != 1 || rows[0].DeliveryMode != mode {
						t.Fatalf("group queue=%+v err=%v", rows, err)
					}
				} else {
					rows, err := repo.PendingDelivery(context.Background(), 3, synthesize, []uint{7, 8})
					if err != nil || len(rows) != 1 || rows[0].DeliveryMode != mode {
						t.Fatalf("task queue=%+v err=%v", rows, err)
					}
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDeliveryQueueIncludesLegacyNullModesAndBoundsQueries(t *testing.T) {
	repo, mock := reportTestDB(t)
	mock.ExpectQuery(`SELECT .* FROM "subagent_tasks" WHERE .*report_status = .* AND status IN .* AND COALESCE\(delivery_mode, ''\) <> .* ORDER BY finished_at ASC,id ASC LIMIT`).WithArgs("pending", "completed", "partial", "failed", "cancelled", "interrupted", "synthesize", 20).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "delivery_mode"}).AddRow("old-task", uint(9), nil))
	tasks, err := repo.PendingDelivery(context.Background(), 1000, false, nil)
	if err != nil || len(tasks) != 1 || tasks[0].DeliveryMode != "" {
		t.Fatalf("legacy task=%+v err=%v", tasks, err)
	}
	mock.ExpectQuery(`SELECT .* FROM "task_groups" WHERE .*report_status = .* AND status IN .* AND COALESCE\(delivery_mode, ''\) <> .* AND COALESCE\(delivery_mode, ''\) <> .* ORDER BY finished_at ASC,id ASC LIMIT`).WithArgs("pending", "completed", "partial", "failed", "cancelled", "interrupted", "synthesize", "direct", 20).WillReturnRows(sqlmock.NewRows([]string{"id", "conversation_id", "delivery_mode"}).AddRow("old-group", uint(9), nil))
	groups, err := repo.PendingGroupDelivery(context.Background(), 0, false, nil)
	if err != nil || len(groups) != 1 || groups[0].DeliveryMode != "" {
		t.Fatalf("legacy group=%+v err=%v", groups, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.PendingDelivery(ctx, 20, false, nil); err == nil {
		t.Fatal("cancelled queue scan accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
