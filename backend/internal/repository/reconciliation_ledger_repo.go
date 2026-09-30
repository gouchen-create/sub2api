package repository

import (
	"context"
	"database/sql"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// reconciliationMaxModelsPerAccount 限制规则页为单个账号展示的模型名数量，
// 避免高频账号把接口响应撑得过大。
const reconciliationMaxModelsPerAccount = 12

// splitReconciliationModels 把 SQL 聚合出的逗号分隔模型名拆成去重切片。
func splitReconciliationModels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	models := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, name)
		if len(models) >= reconciliationMaxModelsPerAccount {
			break
		}
	}
	return models
}

// reconciliationLedgerCTE 构造对账读模型的基础数据集。
//
// 汇总、趋势、明细三个查询全部建立在这份 CTE 之上，因此统计口径与明细列表天然自洽——
// 不会出现「顶部说 10 笔未对账、列表只列出 8 笔」这类自相矛盾的展示。
// 旧实现把同一套判定分别写成一份 SQL 条件与一份 Go 分支，是手工维护的两个副本，容易漂移。
//
// 分类规则（顺序即优先级）：
//  1. 已匹配到上游账单            -> billed（已对账）
//  2. 还没有采集到快照            -> pending（待采集，采集器 30 秒一轮会补上）
//  3. 规则快照为空且当前也无规则  -> rule_unconfigured（规则待配置）
//  4. 对应令牌在窗口内已有账单    -> a6_pending（上游账单待匹配）
//  5. 其余                        -> a6_waiting（等待上游账单）
//
// 加上孤儿账单行（record_type = upstream_unmatched）共六种状态，与前端契约一一对应。
//
// 第 2 步只在快照行缺失时命中：刚产生的调用还没被采集器扫到，
// 这和「账号压根没配规则」是两件不同的事，混在一起会让管理员去配一个本来就配好的规则。
//
// 第 3 步刻意写成「快照 provider 与当前规则 provider 都为空」才算无规则：
// 旧实现用「provider 不等于 subarx」兜底，把「已配置规则、只是账单还没到」误标成
// 「规则待配置」，线上一次误标 1768 条。
//
// 令牌名的取值顺序是「快照优先、当前规则兜底」：管理员把上游令牌改名后，历史账单
// 仍能按调用发生时的快照名匹配上，不会变成孤儿（旧实现只读当前映射，实测影响 1157 条）。
//
// 参数：$1 = from，$2 = to（半开区间 [from, to)）。
const reconciliationLedgerCTE = `
WITH usage_rows AS (
    SELECT
        ul.id AS usage_log_id,
        ul.user_id,
        ul.api_key_id,
        ul.account_id,
        ul.group_id,
        ul.model,
        COALESCE(ul.request_id, '') AS request_id,
        COALESCE(ul.upstream_request_id, '') AS upstream_request_id,
        ul.input_tokens,
        ul.output_tokens,
        (ul.cache_read_tokens + ul.cache_creation_tokens) AS cache_tokens,
        ul.created_at,
        COALESCE(ex.revenue_cny, 0) AS revenue_cny,
        (ex.usage_log_id IS NOT NULL) AS has_snapshot,
        COALESCE(ex.rule_provider, '') AS snapshot_provider,
        COALESCE(ex.rule_external_key, '') AS snapshot_key,
        b.id AS bill_id,
        COALESCE(b.cost_cny, 0) AS bill_cost_cny,
        COALESCE(b.cost_original, 0) AS bill_cost_original,
        COALESCE(b.currency, '') AS bill_currency,
        COALESCE(b.fx_rate_to_cny, 0) AS bill_fx_rate,
        COALESCE(b.match_method, '') AS bill_match_method
    FROM usage_logs ul
    LEFT JOIN reconciliation_usage_extras ex ON ex.usage_log_id = ul.id
    LEFT JOIN reconciliation_upstream_bills b ON b.matched_usage_log_id = ul.id
    WHERE ul.created_at >= $1 AND ul.created_at < $2
),
token_bills AS (
    SELECT token_name, COUNT(*) AS bill_count
    FROM reconciliation_upstream_bills
    WHERE occurred_at >= $1 AND occurred_at < $2
    GROUP BY token_name
),
current_rules AS (
    SELECT account_id, provider, external_key
    FROM reconciliation_account_rules
    WHERE enabled
),
classified AS (
    SELECT
        'downstream' AS record_type,
        u.usage_log_id AS source_id,
        u.created_at,
        u.request_id,
        u.upstream_request_id,
        u.user_id,
        COALESCE(us.email, '') AS user_email,
        u.api_key_id,
        u.account_id,
        u.group_id,
        COALESCE(g.name, '') AS group_name,
        u.model,
        u.input_tokens,
        u.output_tokens,
        u.cache_tokens,
        u.revenue_cny,
        (u.bill_id IS NOT NULL) AS matched,
        u.bill_cost_cny AS upstream_cost_cny,
        u.bill_cost_original AS upstream_cost_original,
        u.bill_currency AS upstream_currency,
        u.bill_fx_rate AS upstream_fx_rate,
        u.bill_match_method AS upstream_match_method,
        CASE
            WHEN u.bill_id IS NOT NULL THEN 'billed'
            WHEN NOT u.has_snapshot THEN 'pending'
            WHEN COALESCE(NULLIF(u.snapshot_provider, ''), r.provider, '') = '' THEN 'rule_unconfigured'
            WHEN COALESCE(tb.bill_count, 0) > 0 THEN 'a6_pending'
            ELSE 'a6_waiting'
        END AS cost_source
    FROM usage_rows u
    LEFT JOIN users us ON us.id = u.user_id
    LEFT JOIN groups g ON g.id = u.group_id
    LEFT JOIN current_rules r ON r.account_id = u.account_id
    LEFT JOIN token_bills tb
           ON COALESCE(NULLIF(u.snapshot_key, ''), r.external_key, '') <> ''
          AND tb.token_name = COALESCE(NULLIF(u.snapshot_key, ''), r.external_key, '')
),
orphan_bills AS (
    SELECT
        'upstream_unmatched' AS record_type,
        0::bigint AS source_id,
        b.occurred_at AS created_at,
        '' AS request_id,
        b.upstream_request_id,
        0::bigint AS user_id,
        '' AS user_email,
        0::bigint AS api_key_id,
        COALESCE(b.matched_account_id, 0) AS account_id,
        0::bigint AS group_id,
        '' AS group_name,
        b.model,
        b.input_tokens,
        b.output_tokens,
        (b.cache_read_tokens + b.cache_creation_tokens) AS cache_tokens,
        0::numeric AS revenue_cny,
        false AS matched,
        b.cost_cny AS upstream_cost_cny,
        b.cost_original AS upstream_cost_original,
        b.currency AS upstream_currency,
        b.fx_rate_to_cny AS upstream_fx_rate,
        b.match_method AS upstream_match_method,
        'upstream_unmatched' AS cost_source
    FROM reconciliation_upstream_bills b
    WHERE b.match_state = 'unmatched'
      AND b.occurred_at >= $1 AND b.occurred_at < $2
),
ledger AS (
    SELECT * FROM classified
    UNION ALL
    SELECT * FROM orphan_bills
)
`

// reconciliationLedgerStatusFilter 把接口层的 status 参数翻译成 WHERE 子句。
//
// 用白名单分支而不是拼接用户输入，既避免 SQL 注入，也保证未知取值退回「全部」。
func reconciliationLedgerStatusFilter(status string) string {
	switch status {
	case "matched":
		return "WHERE record_type = 'downstream' AND matched"
	case "unmatched":
		return "WHERE record_type = 'downstream' AND NOT matched"
	case "upstream_unmatched":
		return "WHERE record_type = 'upstream_unmatched'"
	default:
		return ""
	}
}

type reconciliationLedgerRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

// NewReconciliationLedgerRepository 创建对账读模型仓库。
func NewReconciliationLedgerRepository(client *dbent.Client, sqlDB *sql.DB) service.ReconciliationLedgerRepository {
	return &reconciliationLedgerRepository{client: client, sql: sqlDB}
}

// Summary 汇总窗口内的收入、成本与各类计数。
//
// 收入只统计下游调用行；上游成本只统计「已匹配」的账单，因此
// 毛利恒等于 matched_revenue - upstream_cost，与 profit_scope = matched_only 一致。
// 未匹配的孤儿账单成本不进入合计，但会以独立明细行呈现，管理员仍能看到具体金额。
func (r *reconciliationLedgerRepository) Summary(ctx context.Context, from, to time.Time) (*service.ReconciliationSummary, error) {
	query := reconciliationLedgerCTE + `
SELECT
    COALESCE(SUM(CASE WHEN record_type = 'downstream' THEN revenue_cny END), 0) AS revenue_cny,
    COALESCE(SUM(CASE WHEN record_type = 'downstream' AND matched THEN revenue_cny END), 0) AS matched_revenue_cny,
    COALESCE(SUM(CASE WHEN matched THEN upstream_cost_cny END), 0) AS upstream_cost_cny,
    COUNT(*) FILTER (WHERE record_type = 'downstream' AND matched) AS matched_count,
    COUNT(*) FILTER (WHERE record_type = 'downstream' AND NOT matched) AS unmatched_count,
    COUNT(*) FILTER (WHERE record_type = 'upstream_unmatched') AS upstream_unmatched_count,
    (SELECT COUNT(*) FROM reconciliation_upstream_bills WHERE occurred_at >= $1 AND occurred_at < $2) AS billed_count
FROM ledger
`

	summary := &service.ReconciliationSummary{From: from, To: to}
	if err := scanSingleRow(
		ctx,
		r.sql,
		query,
		[]any{from, to},
		&summary.RevenueCNY,
		&summary.MatchedRevenueCNY,
		&summary.UpstreamCostCNY,
		&summary.Matched,
		&summary.Unmatched,
		&summary.UpstreamUnmatched,
		&summary.BilledCount,
	); err != nil {
		return nil, err
	}
	return summary, nil
}

// Points 按给定分桶粒度返回趋势点。
//
// 分桶用「epoch 取整再还原」实现，不依赖 date_bin，避免对 PostgreSQL 版本的要求。
// 返回的点按时间升序，前端据此推断桶宽。
func (r *reconciliationLedgerRepository) Points(ctx context.Context, from, to time.Time, bucket time.Duration) ([]service.ReconciliationBucketPoint, error) {
	bucketSeconds := int64(bucket / time.Second)
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}

	query := reconciliationLedgerCTE + `
SELECT
    to_timestamp(floor(extract(epoch FROM created_at) / $3) * $3) AS bucket_start,
    COALESCE(SUM(CASE WHEN record_type = 'downstream' THEN revenue_cny END), 0) AS revenue_cny,
    COALESCE(SUM(CASE WHEN matched THEN upstream_cost_cny END), 0) AS upstream_cost_cny,
    COUNT(*) FILTER (WHERE record_type = 'downstream' AND matched) AS matched_count,
    COUNT(*) FILTER (WHERE record_type = 'downstream' AND NOT matched) AS unmatched_count,
    COUNT(*) FILTER (WHERE record_type = 'upstream_unmatched') AS upstream_unmatched_count
FROM ledger
GROUP BY bucket_start
ORDER BY bucket_start
`

	rows, err := r.sql.QueryContext(ctx, query, from, to, bucketSeconds)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	points := make([]service.ReconciliationBucketPoint, 0, 32)
	for rows.Next() {
		var point service.ReconciliationBucketPoint
		if err := rows.Scan(
			&point.Start,
			&point.RevenueCNY,
			&point.UpstreamCostCNY,
			&point.Matched,
			&point.Unmatched,
			&point.UpstreamUnmatched,
		); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return points, nil
}

// Rows 返回明细页与命中总数。
//
// 明细与 Summary 取自同一份 CTE，因此两者永远一致。
func (r *reconciliationLedgerRepository) Rows(ctx context.Context, from, to time.Time, status string, page, pageSize int) ([]service.ReconciliationLedgerRow, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}

	filter := reconciliationLedgerStatusFilter(status)

	countQuery := reconciliationLedgerCTE + `SELECT COUNT(*) FROM ledger ` + filter
	var total int64
	if err := scanSingleRow(ctx, r.sql, countQuery, []any{from, to}, &total); err != nil {
		return nil, 0, err
	}

	listQuery := reconciliationLedgerCTE + `
SELECT
    record_type,
    source_id,
    created_at,
    request_id,
    upstream_request_id,
    user_id,
    user_email,
    api_key_id,
    account_id,
    group_id,
    group_name,
    model,
    input_tokens,
    output_tokens,
    cache_tokens,
    revenue_cny,
    matched,
    upstream_cost_cny,
    upstream_cost_original,
    upstream_currency,
    upstream_fx_rate,
    upstream_match_method,
    cost_source
FROM ledger
` + filter + `
ORDER BY created_at DESC, source_id DESC
LIMIT $3 OFFSET $4
`

	rows, err := r.sql.QueryContext(ctx, listQuery, from, to, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.ReconciliationLedgerRow, 0, pageSize)
	for rows.Next() {
		var row service.ReconciliationLedgerRow
		var costSource string
		if err := rows.Scan(
			&row.RecordType,
			&row.SourceID,
			&row.CreatedAt,
			&row.RequestID,
			&row.UpstreamRequestID,
			&row.UserID,
			&row.UserEmail,
			&row.APIKeyID,
			&row.AccountID,
			&row.GroupID,
			&row.GroupName,
			&row.Model,
			&row.InputTokens,
			&row.OutputTokens,
			&row.CacheTokens,
			&row.RevenueCNY,
			&row.Matched,
			&row.UpstreamCostCNY,
			&row.UpstreamCostOrig,
			&row.UpstreamCurrency,
			&row.UpstreamFxRateCNY,
			&row.UpstreamMatchMethod,
			&costSource,
		); err != nil {
			return nil, 0, err
		}
		row.CostSource = service.ReconciliationCostSource(costSource)
		// 上游成本是否已知：已匹配的调用、以及本身就是上游账单的孤儿行都算已知。
		// 未对账的下游调用必须给出「未知」，接口层据此输出空串让前端显示「—」。
		row.HasUpstreamCost = row.Matched || row.RecordType == service.ReconciliationRecordTypeUpstreamUnmatched
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// UsageCountsByAccount 返回窗口内各账号的调用数、首末调用时间与使用过的模型。
//
// 规则页需要列出「全部账号」，本方法只提供有调用的那部分数据，
// 零调用账号由上层与服务层组合补齐，保证管理员仍能为新账号配置规则。
func (r *reconciliationLedgerRepository) UsageCountsByAccount(ctx context.Context, from, to time.Time) (map[int64]service.ReconciliationAccountUsage, error) {
	query := `
SELECT
    account_id,
    COUNT(*) AS usage_count,
    MIN(created_at) AS first_seen,
    MAX(created_at) AS last_seen,
    COALESCE(string_agg(DISTINCT model, ','), '') AS models
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2 AND account_id IS NOT NULL
GROUP BY account_id
`

	rows, err := r.sql.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]service.ReconciliationAccountUsage)
	for rows.Next() {
		var usage service.ReconciliationAccountUsage
		var models string
		if err := rows.Scan(&usage.AccountID, &usage.Count, &usage.FirstSeen, &usage.LastSeen, &models); err != nil {
			return nil, err
		}
		usage.Models = splitReconciliationModels(models)
		result[usage.AccountID] = usage
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
