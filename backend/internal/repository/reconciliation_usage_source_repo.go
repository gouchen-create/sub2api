package repository

import (
	"context"
	"database/sql"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type reconciliationUsageSourceRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

// NewReconciliationUsageSourceRepository 创建待采集调用的读取仓库。
//
// 它只从 usage_logs 读取采集所需的最小字段，不复制业务口径：
// 用户实付金额始终以 usage_logs.actual_cost 为准。
func NewReconciliationUsageSourceRepository(client *dbent.Client, sqlDB *sql.DB) service.ReconciliationUsageSource {
	return &reconciliationUsageSourceRepository{client: client, sql: sqlDB}
}

// ListUsageBetween 返回 [from, to) 内的调用，按发生时间与主键升序。
//
// 升序保证采集游标可以单调推进；同时按主键排序让同一时刻的多条调用有确定顺序，
// 避免重跑时顺序漂移导致难以复现问题。
func (r *reconciliationUsageSourceRepository) ListUsageBetween(ctx context.Context, from, to time.Time, limit int) ([]service.ReconciliationUsageFact, error) {
	if limit <= 0 {
		limit = 5000
	}

	query := `
SELECT id, account_id, actual_cost, created_at
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2 AND account_id IS NOT NULL
ORDER BY created_at, id
LIMIT $3
`

	rows, err := r.sql.QueryContext(ctx, query, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	facts := make([]service.ReconciliationUsageFact, 0, 256)
	for rows.Next() {
		var fact service.ReconciliationUsageFact
		if err := rows.Scan(&fact.UsageLogID, &fact.AccountID, &fact.ActualCost, &fact.CreatedAt); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}
