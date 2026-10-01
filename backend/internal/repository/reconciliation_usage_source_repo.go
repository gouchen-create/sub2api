package repository

import (
	"context"
	"database/sql"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// reconciliationUsageDefaultLimit 是 ListUsageBetween 的兜底单轮上限。
const reconciliationUsageDefaultLimit = 5000

// reconciliationUsagePendingPredicate 是「这一行还没被采集过」的判定。
//
// 复合游标语义：位置 (boundary_at, boundary_id) 及其之前的行算已采集。
// 写成 `created_at <> $3 OR id > $4` 而不是元组比较 `(created_at, id) > ($3, $4)`，
// 是为了保住游标回退出的那一分钟重扫带——重扫带里的行（created_at < $3）
// 必须继续返回，才能兜住时钟抖动与迟到落库的调用。
const reconciliationUsagePendingPredicate = `created_at >= $1 AND created_at < $2 AND account_id IS NOT NULL
  AND (created_at <> $3 OR id > $4)`

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

// ListUsageBetween 返回窗口内尚未采集的调用，按发生时间与主键升序。
//
// 升序保证采集位置可以单调推进；同时按主键排序让同一时刻的多条调用有确定顺序，
// 这既是「同一 created_at 上的大批量也能分批读完」的前提，也让重跑时的顺序可复现。
//
// where 里的复合游标条件见 reconciliationUsagePendingPredicate：它保证
// 「已采集过的行不再返回」，同时不切断游标回退出来的重扫带。
func (r *reconciliationUsageSourceRepository) ListUsageBetween(ctx context.Context, query service.ReconciliationUsageQuery) ([]service.ReconciliationUsageFact, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = reconciliationUsageDefaultLimit
	}

	sqlQuery := `
SELECT id, account_id, actual_cost, created_at
FROM usage_logs
WHERE ` + reconciliationUsagePendingPredicate + `
ORDER BY created_at, id
LIMIT $5
`

	rows, err := r.sql.QueryContext(ctx, sqlQuery,
		query.From, query.To, query.BoundaryAt, query.BoundaryID, limit)
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

// CountUsagePending 统计窗口内待采集的行数，最多数 limit 行。
//
// 外层再套一层 LIMIT 是关键：游标落后很多时待采集集合可能极大，而积压数字只是
// 一个运维信号，不值得为它做一次全表计数。返回值等于 limit 即表示「至少还有这么多」。
func (r *reconciliationUsageSourceRepository) CountUsagePending(ctx context.Context, query service.ReconciliationUsageQuery, limit int64) (int64, error) {
	if limit <= 0 {
		limit = 1
	}

	sqlQuery := `
SELECT COUNT(*)
FROM (
    SELECT 1
    FROM usage_logs
    WHERE ` + reconciliationUsagePendingPredicate + `
    LIMIT $5
) AS pending
`

	var count int64
	if err := scanSingleRow(ctx, r.sql, sqlQuery,
		[]any{query.From, query.To, query.BoundaryAt, query.BoundaryID, limit}, &count); err != nil {
		return 0, err
	}
	return count, nil
}
