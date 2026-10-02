package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// upstreamCostRepo 是上游成本取数任务在使用记录表上的读写实现。
//
// 只碰 usage_logs 这张表，且只碰 246 号迁移新增的那几列与 upstream_request_id，
// 不改动任何计费口径的列。
type upstreamCostRepo struct {
	db *sql.DB
}

// NewUpstreamCostRepository 创建上游成本取数仓储。
func NewUpstreamCostRepository(db *sql.DB) service.UpstreamCostRepository {
	return &upstreamCostRepo{db: db}
}

// upstreamCostPendingQuery 取一批待查记录。
//
// 条件与 246 号迁移里的部分索引谓词保持一致（带请求 ID + 尚未取到），
// 这样这条查询能直接走那个小索引，而不是扫全表。
//
// ORDER BY created_at ASC 是有意的：按记账时刻先来先查，
// 突发流量把队列撑爆时不会让早先的记录永远排在后面饿死。
const upstreamCostPendingQuery = `
SELECT id, upstream_request_id, upstream_cost_attempts, created_at
FROM usage_logs
WHERE upstream_request_id IS NOT NULL
  AND upstream_request_id <> ''
  AND upstream_cost_fetched_at IS NULL
  AND created_at <= $1
  AND upstream_cost_attempts < $2
ORDER BY created_at ASC
LIMIT $3`

// ListUpstreamCostPending 实现 service.UpstreamCostRepository。
func (r *upstreamCostRepo) ListUpstreamCostPending(
	ctx context.Context,
	now time.Time,
	firstDelay time.Duration,
	maxAttempts int,
	limit int,
) ([]service.UpstreamCostPending, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		return nil, nil
	}
	// 只查「记账时刻已经早于 now-firstDelay」的记录：上游账单落库需要时间，
	// 太早查是注定白跑，还会把重试次数浪费掉。
	cutoff := now.Add(-firstDelay)

	rows, err := r.db.QueryContext(ctx, upstreamCostPendingQuery, cutoff, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("list upstream cost pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.UpstreamCostPending, 0, limit)
	for rows.Next() {
		var item service.UpstreamCostPending
		if err := rows.Scan(&item.UsageLogID, &item.RequestID, &item.Attempts, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan upstream cost pending: %w", err)
		}
		item.RequestID = strings.TrimSpace(item.RequestID)
		// 理论上 SQL 已经把空 ID 挡在外面，这里再挡一次：取数任务唯一的输入
		// 就是这个 ID，放一个空串进去只会换来一次必然失败的请求。
		if item.RequestID == "" {
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upstream cost pending: %w", err)
	}
	return items, nil
}

// upstreamCostResolveQuery 写入取到的成本。
//
// 同时写 fetched_at —— 它是「已取到」的唯一判据，写上有值即代表这条记录从此
// 不再进入待查队列（部分索引也会随之把它移出）。
//
// 只写原币金额与币种，不写任何换算结果：页面上的「费用」就是美元原值，
// 两者同币种直接相减才是主人要看的毛利。
const upstreamCostResolveQuery = `
UPDATE usage_logs
SET upstream_cost_original   = $2,
    upstream_cost_currency   = $3,
    upstream_cost_fetched_at = $4
WHERE id = $1`

// ResolveUpstreamCost 实现 service.UpstreamCostRepository。
func (r *upstreamCostRepo) ResolveUpstreamCost(
	ctx context.Context,
	usageLogID int64,
	value service.UpstreamCostValue,
	at time.Time,
) error {
	if r == nil || r.db == nil {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, upstreamCostResolveQuery,
		usageLogID, value.Original, value.Currency, at); err != nil {
		return fmt.Errorf("resolve upstream cost: %w", err)
	}
	return nil
}

// upstreamCostRetryQuery 记一次「查了但没有」。
//
// 只加计数、不写 fetched_at：这条记录下一轮仍会被捞出来重试，
// 直到计数达到上限后自然退出队列（呈现为「未取到」）。
const upstreamCostRetryQuery = `
UPDATE usage_logs
SET upstream_cost_attempts = upstream_cost_attempts + 1
WHERE id = $1`

// RetryUpstreamCost 实现 service.UpstreamCostRepository。
func (r *upstreamCostRepo) RetryUpstreamCost(ctx context.Context, usageLogID int64) error {
	if r == nil || r.db == nil {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, upstreamCostRetryQuery, usageLogID); err != nil {
		return fmt.Errorf("retry upstream cost: %w", err)
	}
	return nil
}
