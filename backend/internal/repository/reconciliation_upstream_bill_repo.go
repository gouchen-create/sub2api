package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// reconciliationBillInsertColumns 是导入上游账单时的写入列。
//
// 刻意不包含 imported_at（由建表时的 DEFAULT now() 填充）与匹配结果列（由匹配流程单独更新）。
// 注意：VALUES 模板必须与这里的列数严格相等，多写一个表达式（例如给已排除的列补 now()）
// PostgreSQL 会直接以 "INSERT has more expressions than target columns" 拒绝整条语句。
const reconciliationBillInsertColumns = `provider, upstream_request_id, occurred_at, billing_date, model, token_name,
    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cache_tokens_total,
    cost_original, currency, fx_rate_to_cny, cost_cny, source, raw`

// reconciliationBillInsertColumnCount 与上面的列数保持一致，改动列清单时必须同步。
const reconciliationBillInsertColumnCount = 17

// reconciliationBillInsertChunk 限制单条 INSERT 的行数。
// 每行 17 个占位符，1000 行约 1.7 万个参数，远低于 PostgreSQL 的参数上限。
const reconciliationBillInsertChunk = 1000

// reconciliationMatchCandidateColumns 是匹配候选查询的统一列清单。
const reconciliationMatchCandidateColumns = `ul.id, ul.account_id, ul.model,
    COALESCE(ul.upstream_request_id, ''), ul.input_tokens, ul.output_tokens,
    ul.cache_read_tokens, ul.cache_creation_tokens, ul.created_at`

type reconciliationUpstreamBillRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

// NewReconciliationUpstreamBillRepository 创建上游账单仓库。
func NewReconciliationUpstreamBillRepository(client *dbent.Client, sqlDB *sql.DB) service.ReconciliationUpstreamBillRepository {
	return &reconciliationUpstreamBillRepository{client: client, sql: sqlDB}
}

// UpsertBatch 幂等导入上游账单，返回新增条数。
//
// 冲突（同一 provider + upstream_request_id）时用 DO NOTHING 整行跳过：
// 成本与汇率在首次导入时冻结，重复导入绝不能覆盖它们，否则历史的
// cost_cny 会随着汇率调整而漂移，已经对好的账会对不上。
func (r *reconciliationUpstreamBillRepository) UpsertBatch(ctx context.Context, bills []service.ReconciliationUpstreamBillPayload) (int64, error) {
	if len(bills) == 0 {
		return 0, nil
	}

	var inserted int64
	for start := 0; start < len(bills); start += reconciliationBillInsertChunk {
		end := min(start+reconciliationBillInsertChunk, len(bills))
		count, err := r.insertChunk(ctx, bills[start:end])
		if err != nil {
			return inserted, err
		}
		inserted += count
	}
	return inserted, nil
}

func (r *reconciliationUpstreamBillRepository) insertChunk(ctx context.Context, bills []service.ReconciliationUpstreamBillPayload) (int64, error) {
	placeholders := make([]string, 0, len(bills))
	args := make([]any, 0, len(bills)*reconciliationBillInsertColumnCount)

	for i := range bills {
		bill := &bills[i]
		base := i * reconciliationBillInsertColumnCount
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d::jsonb)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
			base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16, base+17,
		))

		raw := any(nil)
		if len(bill.Raw) > 0 {
			encoded, err := json.Marshal(bill.Raw)
			if err != nil {
				// 原始载荷只用于追溯，编码失败不应该让整批账单导入失败。
				encoded = nil
			}
			if len(encoded) > 0 {
				raw = string(encoded)
			}
		}

		var billingDate any
		if bill.BillingDate != nil {
			billingDate = *bill.BillingDate
		}

		currency := bill.Currency
		if strings.TrimSpace(currency) == "" {
			currency = "USD"
		}
		source := bill.Source
		if strings.TrimSpace(source) == "" {
			source = "a6"
		}

		args = append(args,
			bill.Provider,
			bill.UpstreamRequestID,
			bill.OccurredAt,
			billingDate,
			bill.Model,
			bill.TokenName,
			bill.InputTokens,
			bill.OutputTokens,
			bill.CacheReadTokens,
			bill.CacheCreationTokens,
			bill.CacheTokensTotal,
			bill.CostOriginal,
			currency,
			bill.FxRateToCNY,
			bill.CostCNY,
			source,
			raw,
		)
	}

	query := `INSERT INTO reconciliation_upstream_bills (` + reconciliationBillInsertColumns + `)
VALUES ` + strings.Join(placeholders, ",") + `
ON CONFLICT (provider, upstream_request_id) DO NOTHING`

	result, err := r.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, translatePersistenceError(err, nil, nil)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// ListStaging 取出尚未完成首次匹配的账单。
//
// 按发生时间升序：先到的账单先匹配，符合真实结算顺序，也让失败重试的推进可见。
func (r *reconciliationUpstreamBillRepository) ListStaging(ctx context.Context, from, to time.Time, limit int) ([]service.ReconciliationUpstreamBill, error) {
	if limit <= 0 {
		limit = 1000
	}

	query := `
SELECT id, provider, upstream_request_id, occurred_at, billing_date, model, token_name,
       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cache_tokens_total,
       cost_original, currency, fx_rate_to_cny, cost_cny, source
FROM reconciliation_upstream_bills
WHERE match_state = 'staging' AND occurred_at >= $1 AND occurred_at < $2
ORDER BY occurred_at, id
LIMIT $3
`

	rows, err := r.sql.QueryContext(ctx, query, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	bills := make([]service.ReconciliationUpstreamBill, 0, limit)
	for rows.Next() {
		var bill service.ReconciliationUpstreamBill
		var payload service.ReconciliationUpstreamBillPayload
		var billingDate sql.NullTime
		if err := rows.Scan(
			&bill.ID,
			&payload.Provider,
			&payload.UpstreamRequestID,
			&payload.OccurredAt,
			&billingDate,
			&payload.Model,
			&payload.TokenName,
			&payload.InputTokens,
			&payload.OutputTokens,
			&payload.CacheReadTokens,
			&payload.CacheCreationTokens,
			&payload.CacheTokensTotal,
			&payload.CostOriginal,
			&payload.Currency,
			&payload.FxRateToCNY,
			&payload.CostCNY,
			&payload.Source,
		); err != nil {
			return nil, err
		}
		if billingDate.Valid {
			value := billingDate.Time
			payload.BillingDate = &value
		}
		bill.Payload = payload
		bills = append(bills, bill)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bills, nil
}

// FindDirectMatchCandidates 按上游请求 ID 找出可匹配的下游调用。
//
// 已挂过账单的调用会被排除：唯一部分索引会拒绝第二次绑定，
// 提前排除可以让匹配流程把候选数算准，避免把「唯一候选」误判成并列。
func (r *reconciliationUpstreamBillRepository) FindDirectMatchCandidates(ctx context.Context, upstreamRequestID string) ([]service.ReconciliationMatchCandidate, error) {
	if strings.TrimSpace(upstreamRequestID) == "" {
		return nil, nil
	}

	query := `
SELECT ` + reconciliationMatchCandidateColumns + `
FROM usage_logs ul
WHERE ul.upstream_request_id = $1
  AND NOT EXISTS (
      SELECT 1 FROM reconciliation_upstream_bills b WHERE b.matched_usage_log_id = ul.id
  )
ORDER BY ul.created_at, ul.id
`

	return r.scanMatchCandidates(ctx, query, upstreamRequestID)
}

// FindCompositeMatchCandidates 按账号范围、模型、输出 token 与时间窗口粗筛候选调用。
//
// 缓存与输入 token 的逐级判定由 service 层完成，这里不做过滤。
func (r *reconciliationUpstreamBillRepository) FindCompositeMatchCandidates(ctx context.Context, params service.ReconciliationCompositeQuery) ([]service.ReconciliationMatchCandidate, error) {
	if len(params.AccountIDs) == 0 {
		return nil, nil
	}

	window := params.TimeWindow
	if window <= 0 {
		window = 2 * time.Minute
	}
	from := params.OccurredAt.Add(-window)
	to := params.OccurredAt.Add(window)

	placeholders := make([]string, 0, len(params.AccountIDs))
	args := make([]any, 0, len(params.AccountIDs)+4)
	for i, accountID := range params.AccountIDs {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, accountID)
	}
	args = append(args, params.Model, params.OutputTokens, from, to)
	modelPos := len(params.AccountIDs) + 1

	query := `
SELECT ` + reconciliationMatchCandidateColumns + `
FROM usage_logs ul
WHERE ul.account_id IN (` + strings.Join(placeholders, ",") + `)
  AND ul.model = $` + fmt.Sprintf("%d", modelPos) + `
  AND ul.output_tokens = $` + fmt.Sprintf("%d", modelPos+1) + `
  AND ul.created_at >= $` + fmt.Sprintf("%d", modelPos+2) + `
  AND ul.created_at <= $` + fmt.Sprintf("%d", modelPos+3) + `
  AND NOT EXISTS (
      SELECT 1 FROM reconciliation_upstream_bills b WHERE b.matched_usage_log_id = ul.id
  )
ORDER BY ul.created_at, ul.id
`

	return r.scanMatchCandidates(ctx, query, args...)
}

func (r *reconciliationUpstreamBillRepository) scanMatchCandidates(ctx context.Context, query string, args ...any) ([]service.ReconciliationMatchCandidate, error) {
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	candidates := make([]service.ReconciliationMatchCandidate, 0, 8)
	for rows.Next() {
		var candidate service.ReconciliationMatchCandidate
		if err := rows.Scan(
			&candidate.UsageLogID,
			&candidate.AccountID,
			&candidate.Model,
			&candidate.UpstreamRequestID,
			&candidate.InputTokens,
			&candidate.OutputTokens,
			&candidate.CacheReadTokens,
			&candidate.CacheCreationTok,
			&candidate.CreatedAt,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

// MarkMatched 把账单标记为已匹配。
//
// WHERE 上带 matched_usage_log_id IS NULL：并发下若同一账单被两条流程同时匹配，
// 只有一个能成功，另一个影响行数为 0，由调用方识别为竞态并重新取候选。
func (r *reconciliationUpstreamBillRepository) MarkMatched(ctx context.Context, billID, usageLogID, accountID int64, method string) error {
	query := `
UPDATE reconciliation_upstream_bills
SET match_state = 'matched',
    match_method = $2,
    matched_usage_log_id = $3,
    matched_account_id = $4,
    updated_at = now()
WHERE id = $1 AND matched_usage_log_id IS NULL
`
	_, err := r.sql.ExecContext(ctx, query, billID, method, usageLogID, accountID)
	return translatePersistenceError(err, nil, nil)
}

// MarkUnmatched 把账单标记为确认匹配不上。
//
// 只处理 staging 状态的账单：已经匹配成功的账单不会被降级，
// 否则一次失败的补匹配会把已完成的对账结果抹掉。
func (r *reconciliationUpstreamBillRepository) MarkUnmatched(ctx context.Context, billIDs []int64) error {
	if len(billIDs) == 0 {
		return nil
	}

	placeholders := make([]string, 0, len(billIDs))
	args := make([]any, 0, len(billIDs))
	for i, billID := range billIDs {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, billID)
	}

	query := `
UPDATE reconciliation_upstream_bills
SET match_state = 'unmatched', updated_at = now()
WHERE id IN (` + strings.Join(placeholders, ",") + `) AND match_state = 'staging'
`
	_, err := r.sql.ExecContext(ctx, query, args...)
	return translatePersistenceError(err, nil, nil)
}

// ProviderTokenNames 返回窗口内出现过账单的上游令牌名及各自账单数。
func (r *reconciliationUpstreamBillRepository) ProviderTokenNames(ctx context.Context, from, to time.Time) (map[string]int64, error) {
	query := `
SELECT token_name, COUNT(*)
FROM reconciliation_upstream_bills
WHERE occurred_at >= $1 AND occurred_at < $2
GROUP BY token_name
`

	rows, err := r.sql.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var tokenName string
		var count int64
		if err := rows.Scan(&tokenName, &count); err != nil {
			return nil, err
		}
		result[tokenName] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// CountUnmatched 统计窗口内孤儿账单数。
func (r *reconciliationUpstreamBillRepository) CountUnmatched(ctx context.Context, from, to time.Time) (int64, error) {
	query := `
SELECT COUNT(*)
FROM reconciliation_upstream_bills
WHERE match_state = 'unmatched' AND occurred_at >= $1 AND occurred_at < $2
`
	var count int64
	if err := scanSingleRow(ctx, r.sql, query, []any{from, to}, &count); err != nil {
		return 0, err
	}
	return count, nil
}
