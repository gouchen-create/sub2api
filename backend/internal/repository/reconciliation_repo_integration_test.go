//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// ==================== 隔离窗口 ====================
//
// 读模型（Summary / Points / Rows）只按时间窗口过滤 usage_logs 与
// reconciliation_upstream_bills，没有天然的数据隔离：同包其它测试往
// usage_logs 写一条 now() 的记录，就会串进「全部明细」里，让
// 「顶部说 6 笔、列表列出 7 笔」这类断言变成随机失败。
//
// 因此每个用例通过 newReconFixture 领一个互不重叠的整点小时窗口，
// 并把自己的全部事实（调用、快照、账单、规则）写进这个窗口。
// 窗口从 2001-01-01T00:00:00Z 起按小时递增，既远离 now()（不会与其它
// 测试的实时写入重叠），也不需要建表或改表。
var reconTestEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// reconWindowSeq 保证同一进程内的窗口两两不同；测试不并行，这里只是多加一层保险。
var reconWindowSeq atomic.Int64

// reconFixture 是一个用例的隔离环境：一个时间窗口 + 一套父级实体 + 清理逻辑。
type reconFixture struct {
	t      *testing.T
	ctx    context.Context
	client *dbent.Client
	db     *sql.DB

	from time.Time
	to   time.Time

	userID     int64
	groupID    int64
	apiKeyID   int64
	accountIDs []int64
}

func newReconFixture(t *testing.T) *reconFixture {
	t.Helper()

	ctx := context.Background()
	client := testEntClient(t)

	from := reconTestEpoch.Add(time.Duration(reconWindowSeq.Add(1)) * time.Hour)
	to := from.Add(time.Hour)

	f := &reconFixture{
		t:      t,
		ctx:    ctx,
		client: client,
		db:     integrationDB,
		from:   from,
		to:     to,
	}

	// usage_logs 上 user_id / api_key_id / account_id 都是 NOT NULL 且带外键，
	// 所以父级实体必须自己造，不能指望库里已经有现成的用户或分组。
	// 名字里带上窗口序号，避免与同包其它测试撞唯一约束。
	user := mustCreateUser(t, client, &service.User{
		Email: fmt.Sprintf("recon-%d@example.com", from.Unix()),
	})
	f.userID = user.ID

	group := mustCreateGroup(t, client, &service.Group{
		Name: fmt.Sprintf("recon-group-%d", from.Unix()),
	})
	f.groupID = group.ID

	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    fmt.Sprintf("sk-recon-%d", from.Unix()),
		Name:   "recon-integration",
	})
	f.apiKeyID = apiKey.ID

	t.Cleanup(f.cleanup)
	return f
}

// cleanup 按「先子表后主表」的顺序删掉本窗口内的一切。
//
// reconciliation_* 三张表刻意没有外键，数据库不会帮忙级联，必须自己动手；
// usage_logs 对 users / api_keys / accounts 有外键，所以它们排在最后。
func (f *reconFixture) cleanup() {
	ctx := context.Background()

	steps := []struct {
		query string
		args  []any
	}{
		{
			`DELETE FROM reconciliation_usage_extras WHERE usage_log_id IN (
			     SELECT id FROM usage_logs WHERE created_at >= $1 AND created_at < $2)`,
			[]any{f.from, f.to},
		},
		{
			`DELETE FROM reconciliation_upstream_bills WHERE occurred_at >= $1 AND occurred_at < $2`,
			[]any{f.from, f.to},
		},
		{
			`DELETE FROM reconciliation_account_rules WHERE account_id = ANY($1)`,
			[]any{pq.Array(f.accountIDs)},
		},
		{
			`DELETE FROM usage_logs WHERE created_at >= $1 AND created_at < $2`,
			[]any{f.from, f.to},
		},
		{`DELETE FROM api_keys WHERE id = $1`, []any{f.apiKeyID}},
		{`DELETE FROM accounts WHERE id = ANY($1)`, []any{pq.Array(f.accountIDs)}},
		{`DELETE FROM groups WHERE id = $1`, []any{f.groupID}},
		{`DELETE FROM users WHERE id = $1`, []any{f.userID}},
	}

	for _, step := range steps {
		if _, err := f.db.ExecContext(ctx, step.query, step.args...); err != nil {
			f.t.Errorf("recon fixture cleanup failed: %v (query=%s)", err, step.query)
		}
	}
}

// uniq 给字符串加上本窗口的后缀。
//
// 不是所有查询都被时间窗口约束：FindDirectMatchCandidates 按上游请求 ID 全表查，
// ListAccountIDsByRuleKeys 按令牌名全表查（这是刻意的设计，令牌改名后要能回查历史）。
// 这类查询的输入必须全局唯一，否则会读到别的用例留下的行。
func (f *reconFixture) uniq(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, f.from.Unix())
}

// at 返回窗口内第 minutes 分钟，用于给调用与账单排序。
func (f *reconFixture) at(minutes int) time.Time {
	return f.from.Add(time.Duration(minutes) * time.Minute)
}

func (f *reconFixture) newAccount(name string) int64 {
	f.t.Helper()
	account := mustCreateAccount(f.t, f.client, &service.Account{Name: fmt.Sprintf("%s-%d", name, f.from.Unix())})
	f.accountIDs = append(f.accountIDs, account.ID)
	return account.ID
}

func (f *reconFixture) ledgerRepo() service.ReconciliationLedgerRepository {
	return NewReconciliationLedgerRepository(f.client, f.db)
}

func (f *reconFixture) billRepo() service.ReconciliationUpstreamBillRepository {
	return NewReconciliationUpstreamBillRepository(f.client, f.db)
}

func (f *reconFixture) extraRepo() service.ReconciliationUsageExtraRepository {
	return NewReconciliationUsageExtraRepository(f.client)
}

// ==================== 事实写入 ====================

type reconUsageSpec struct {
	accountID         int64
	model             string
	requestID         string
	upstreamRequestID string
	inputTokens       int
	outputTokens      int
	cacheReadTokens   int
	cacheCreation     int
	actualCost        float64
	at                time.Time
}

// insertUsage 插一条下游调用。
//
// 列名取自 migrations/001_init.sql 的 usage_logs 定义与后续 ALTER：
// 真正 NOT NULL 且无默认值的只有 user_id / api_key_id / account_id / model，
// 其余列都有默认值；这里仍然显式填 token 数、actual_cost 与 created_at，
// 让事实行读起来是一笔真实调用而不是一堆默认值。
func (f *reconFixture) insertUsage(spec reconUsageSpec) int64 {
	f.t.Helper()

	require.Truef(f.t,
		!spec.at.Before(f.from) && spec.at.Before(f.to),
		"usage 时间 %s 必须落在隔离窗口 [%s, %s) 内", spec.at, f.from, f.to)

	model := spec.model
	if model == "" {
		model = "claude-sonnet-4-5"
	}

	var id int64
	err := scanSingleRow(f.ctx, f.db, `
INSERT INTO usage_logs (
    user_id, api_key_id, account_id, group_id, model, request_id, upstream_request_id,
    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
    total_cost, actual_cost, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,$13)
RETURNING id`,
		[]any{
			f.userID, f.apiKeyID, spec.accountID, f.groupID, model,
			spec.requestID, spec.upstreamRequestID,
			spec.inputTokens, spec.outputTokens, spec.cacheReadTokens, spec.cacheCreation,
			spec.actualCost, spec.at.UTC(),
		},
		&id,
	)
	require.NoError(f.t, err, "insert usage_logs")
	return id
}

// insertExtra 插一条调用侧快照。
//
// 读模型只读 revenue_cny（收入原值与汇率原样留在快照里），因此这里让
// revenue_original = revenue_cny、fx_rate_to_cny = 1，保持金额可读。
func (f *reconFixture) insertExtra(usageLogID, accountID int64, ruleProvider, ruleExternalKey string, revenueCNY float64) {
	f.t.Helper()

	_, err := f.db.ExecContext(f.ctx, `
INSERT INTO reconciliation_usage_extras (
    usage_log_id, account_id, rule_provider, rule_external_key, rule_version,
    revenue_original, fx_rate_to_cny, revenue_cny, collected_at
) VALUES ($1,$2,$3,$4,1,$5,1,$5,now())`,
		usageLogID, accountID, ruleProvider, ruleExternalKey, revenueCNY)
	require.NoError(f.t, err, "insert reconciliation_usage_extras")
}

// setRule 写一条账号规则；已存在则整体覆盖（version 自增，与 Upsert 语义一致）。
func (f *reconFixture) setRule(accountID int64, provider, externalKey string, enabled bool) {
	f.t.Helper()

	_, err := f.db.ExecContext(f.ctx, `
INSERT INTO reconciliation_account_rules (account_id, provider, external_key, version, enabled)
VALUES ($1,$2,$3,1,$4)
ON CONFLICT (account_id) DO UPDATE
SET provider = EXCLUDED.provider,
    external_key = EXCLUDED.external_key,
    enabled = EXCLUDED.enabled,
    version = reconciliation_account_rules.version + 1,
    updated_at = now()`,
		accountID, provider, externalKey, enabled)
	require.NoError(f.t, err, "upsert reconciliation_account_rules")
}

// renameRuleKey 模拟管理员在上游把令牌改名后重新保存规则。
func (f *reconFixture) renameRuleKey(accountID int64, externalKey string) {
	f.t.Helper()

	_, err := f.db.ExecContext(f.ctx, `
UPDATE reconciliation_account_rules
SET external_key = $2, version = version + 1, updated_at = now()
WHERE account_id = $1`, accountID, externalKey)
	require.NoError(f.t, err, "rename reconciliation_account_rules.external_key")
}

type reconBillSpec struct {
	provider          string
	upstreamRequestID string
	occurredAt        time.Time
	model             string
	tokenName         string
	inputTokens       int
	outputTokens      int
	cacheReadTokens   int
	cacheCreation     int
	cacheTokensTotal  int
	costOriginal      float64
	currency          string
	fxRateToCNY       float64
	costCNY           float64
	matchState        string
	matchMethod       string
	matchedUsageLogID int64
	matchedAccountID  int64
}

// insertBill 直接写一条上游账单，用于精确控制 match_state 与匹配结果。
// 导入路径本身的幂等语义由 UpsertBatch 的用例覆盖，这里只造事实。
func (f *reconFixture) insertBill(spec reconBillSpec) int64 {
	f.t.Helper()

	require.Truef(f.t,
		!spec.occurredAt.Before(f.from) && spec.occurredAt.Before(f.to),
		"bill 时间 %s 必须落在隔离窗口 [%s, %s) 内", spec.occurredAt, f.from, f.to)

	provider := spec.provider
	if provider == "" {
		provider = "a6"
	}
	currency := spec.currency
	if currency == "" {
		currency = "USD"
	}
	matchState := spec.matchState
	if matchState == "" {
		matchState = "staging"
	}

	var matchedUsageLogID any
	if spec.matchedUsageLogID > 0 {
		matchedUsageLogID = spec.matchedUsageLogID
	}
	var matchedAccountID any
	if spec.matchedAccountID > 0 {
		matchedAccountID = spec.matchedAccountID
	}

	var id int64
	err := scanSingleRow(f.ctx, f.db, `
INSERT INTO reconciliation_upstream_bills (
    provider, upstream_request_id, occurred_at, model, token_name,
    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cache_tokens_total,
    cost_original, currency, fx_rate_to_cny, cost_cny, source,
    match_state, match_method, matched_usage_log_id, matched_account_id
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'a6',$15,$16,$17,$18)
RETURNING id`,
		[]any{
			provider, spec.upstreamRequestID, spec.occurredAt.UTC(), spec.model, spec.tokenName,
			spec.inputTokens, spec.outputTokens, spec.cacheReadTokens, spec.cacheCreation, spec.cacheTokensTotal,
			spec.costOriginal, currency, spec.fxRateToCNY, spec.costCNY,
			matchState, spec.matchMethod, matchedUsageLogID, matchedAccountID,
		},
		&id,
	)
	require.NoError(f.t, err, "insert reconciliation_upstream_bills")
	return id
}

// requireNoExtra 断言一条调用确实还没有采集到快照行（pending 的前提条件）。
func (f *reconFixture) requireNoExtra(usageLogID int64) {
	f.t.Helper()

	var count int64
	require.NoError(f.t, scanSingleRow(f.ctx, f.db,
		`SELECT COUNT(*) FROM reconciliation_usage_extras WHERE usage_log_id = $1`,
		[]any{usageLogID}, &count))
	require.Zerof(f.t, count, "usage_log %d 不应有快照行", usageLogID)
}

// billState 读回一条账单的状态机字段。
func (f *reconFixture) billState(id int64) (matchState, matchMethod string, matchedUsageLogID int64) {
	f.t.Helper()

	var matched sql.NullInt64
	require.NoError(f.t, scanSingleRow(f.ctx, f.db,
		`SELECT match_state, match_method, matched_usage_log_id
		 FROM reconciliation_upstream_bills WHERE id = $1`,
		[]any{id}, &matchState, &matchMethod, &matched))
	if matched.Valid {
		matchedUsageLogID = matched.Int64
	}
	return matchState, matchMethod, matchedUsageLogID
}

// rowsByRequestID 把明细切成「下游行按 request_id 索引」与「孤儿行」两部分。
func splitLedgerRows(t *testing.T, rows []service.ReconciliationLedgerRow) (map[string]service.ReconciliationLedgerRow, []service.ReconciliationLedgerRow) {
	t.Helper()

	downstream := make(map[string]service.ReconciliationLedgerRow, len(rows))
	orphans := make([]service.ReconciliationLedgerRow, 0)
	for _, row := range rows {
		if row.RecordType == service.ReconciliationRecordTypeUpstreamUnmatched {
			orphans = append(orphans, row)
			continue
		}
		downstream[row.RequestID] = row
	}
	return downstream, orphans
}

// ==================== A. 六种对账状态的分类 ====================
//
// Summary / Rows / Points 共用 reconciliationLedgerCTE，这里用 Rows 把六种取值
// 一次性验穿：billed / pending / rule_unconfigured / a6_pending / a6_waiting /
// upstream_unmatched。
func TestReconciliationLedgerRows_CostSourceClassification(t *testing.T) {
	f := newReconFixture(t)

	billedAccount := f.newAccount("recon-a-billed")
	pendingAccount := f.newAccount("recon-a-pending")
	noRuleAccount := f.newAccount("recon-a-norule")
	a6PendingAccount := f.newAccount("recon-a-a6pending")
	a6WaitingAccount := f.newAccount("recon-a-a6waiting")

	// 1) billed：已有账单的 matched_usage_log_id 指回这条调用。
	billedLog := f.insertUsage(reconUsageSpec{
		accountID: billedAccount, requestID: "req-billed", actualCost: 1.5, at: f.at(1),
	})
	f.insertExtra(billedLog, billedAccount, "a6", f.uniq("tok-billed"), 10)
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-billed"), occurredAt: f.at(1), tokenName: f.uniq("tok-billed"),
		costOriginal: 4, fxRateToCNY: 1, costCNY: 4,
		matchState: "matched", matchMethod: "direct",
		matchedUsageLogID: billedLog, matchedAccountID: billedAccount,
	})

	// 2) pending：采集器还没扫到，根本没有快照行。
	pendingLog := f.insertUsage(reconUsageSpec{
		accountID: pendingAccount, requestID: "req-pending", actualCost: 2, at: f.at(2),
	})
	f.requireNoExtra(pendingLog)

	// 3) rule_unconfigured：有快照，但规则快照为空且该账号当前也没有启用的规则。
	noRuleLog := f.insertUsage(reconUsageSpec{
		accountID: noRuleAccount, requestID: "req-norule", actualCost: 3, at: f.at(3),
	})
	f.insertExtra(noRuleLog, noRuleAccount, "", "", 3)

	// 4) a6_pending：规则配好了，窗口内也有该令牌名的账单，但没匹配到这条调用。
	a6PendingLog := f.insertUsage(reconUsageSpec{
		accountID: a6PendingAccount, requestID: "req-a6pending", actualCost: 4, at: f.at(4),
	})
	f.setRule(a6PendingAccount, "a6", f.uniq("tok-a6pending"), true)
	f.insertExtra(a6PendingLog, a6PendingAccount, "a6", f.uniq("tok-a6pending"), 4)
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-a6pending"), occurredAt: f.at(4),
		tokenName: f.uniq("tok-a6pending"), costCNY: 5, // 仍是 staging，没匹配到任何调用
	})

	// 5) a6_waiting：规则配好了，但窗口内还没有该令牌名的账单。
	a6WaitingLog := f.insertUsage(reconUsageSpec{
		accountID: a6WaitingAccount, requestID: "req-a6waiting", actualCost: 5, at: f.at(5),
	})
	f.setRule(a6WaitingAccount, "a6", f.uniq("tok-a6waiting"), true)
	f.insertExtra(a6WaitingLog, a6WaitingAccount, "a6", f.uniq("tok-a6waiting"), 5)

	// 6) upstream_unmatched：孤儿账单行（match_state = 'unmatched'）。
	orphanRequestID := f.uniq("up-orphan")
	f.insertBill(reconBillSpec{
		upstreamRequestID: orphanRequestID, occurredAt: f.at(6),
		model: "claude-opus-4-1", tokenName: f.uniq("tok-orphan"),
		inputTokens: 30, outputTokens: 40, cacheReadTokens: 5, cacheCreation: 6, cacheTokensTotal: 11,
		costOriginal: 7, fxRateToCNY: 1, costCNY: 7, matchState: "unmatched",
	})

	rows, total, err := f.ledgerRepo().Rows(f.ctx, f.from, f.to, "", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 6, total, "5 条下游调用 + 1 条孤儿账单")
	require.Len(t, rows, 6)

	downstream, orphans := splitLedgerRows(t, rows)
	require.Len(t, downstream, 5)

	// 1) billed
	billed, ok := downstream["req-billed"]
	require.True(t, ok, "billed 行必须出现")
	require.Equal(t, service.ReconciliationCostSourceBilled, billed.CostSource)
	require.Equal(t, service.ReconciliationRecordTypeDownstream, billed.RecordType)
	require.True(t, billed.Matched)
	require.True(t, billed.HasUpstreamCost)
	require.InDelta(t, 10, billed.RevenueCNY, 1e-9)
	require.InDelta(t, 4, billed.UpstreamCostCNY, 1e-9)
	require.Equal(t, "direct", billed.UpstreamMatchMethod)

	// 2) pending
	pending, ok := downstream["req-pending"]
	require.True(t, ok)
	require.Equal(t, service.ReconciliationCostSourcePending, pending.CostSource)
	require.False(t, pending.Matched)
	require.False(t, pending.HasUpstreamCost, "还没对账的调用必须给出「成本未知」")
	require.Zero(t, pending.RevenueCNY, "没有快照就没有冻结收入")

	// 3) rule_unconfigured
	noRule, ok := downstream["req-norule"]
	require.True(t, ok)
	require.Equal(t, service.ReconciliationCostSourceRuleUnconfigured, noRule.CostSource)
	require.False(t, noRule.HasUpstreamCost)

	// 4) a6_pending
	a6Pending, ok := downstream["req-a6pending"]
	require.True(t, ok)
	require.Equal(t, service.ReconciliationCostSourceA6Pending, a6Pending.CostSource)
	require.False(t, a6Pending.Matched)
	require.False(t, a6Pending.HasUpstreamCost)

	// 5) a6_waiting
	a6Waiting, ok := downstream["req-a6waiting"]
	require.True(t, ok)
	require.Equal(t, service.ReconciliationCostSourceA6Waiting, a6Waiting.CostSource)
	require.False(t, a6Waiting.HasUpstreamCost)

	// 6) upstream_unmatched：孤儿账单行
	require.Len(t, orphans, 1)
	orphan := orphans[0]
	require.Equal(t, service.ReconciliationCostSourceUpstreamUnmatched, orphan.CostSource)
	require.Equal(t, service.ReconciliationRecordTypeUpstreamUnmatched, orphan.RecordType)
	require.Zero(t, orphan.SourceID, "孤儿行没有下游主键，source_id 必须是 0")
	require.Equal(t, orphanRequestID, orphan.UpstreamRequestID)
	require.Equal(t, "claude-opus-4-1", orphan.Model)
	require.EqualValues(t, 30, orphan.InputTokens)
	require.EqualValues(t, 40, orphan.OutputTokens)
	require.EqualValues(t, 11, orphan.CacheTokens)
	require.False(t, orphan.Matched, "孤儿账单没有下游收入，不算已对账")
	require.True(t, orphan.HasUpstreamCost, "孤儿行的成本是已知的")
	require.InDelta(t, 7, orphan.UpstreamCostCNY, 1e-9)
	require.InDelta(t, 7, orphan.UpstreamCostOrig, 1e-9)
	require.Equal(t, "USD", orphan.UpstreamCurrency)
	require.InDelta(t, 1, orphan.UpstreamFxRateCNY, 1e-9)
	require.Zero(t, orphan.RevenueCNY, "孤儿行没有下游收入")
	require.Zero(t, orphan.UserID)
	require.Zero(t, orphan.GroupID)
	require.Empty(t, orphan.UserEmail)
	require.Empty(t, orphan.GroupName)
}

// ==================== B. Bug 1 回归：rule_unconfigured 不得误标 ====================
//
// 旧实现用「provider 不等于某上游」兜底，把「账号已配规则、只是账单还没到」
// 误标成「规则待配置」，线上一次误标 1768 条。
func TestReconciliationLedgerRows_Bug1_ConfiguredRuleWithoutBillIsNotRuleUnconfigured(t *testing.T) {
	f := newReconFixture(t)
	account := f.newAccount("recon-bug1")

	// 账号已配置规则，窗口内还没有该令牌名的账单。
	f.setRule(account, "a6", f.uniq("tok-bug1"), true)

	// 变体一：正常采集路径，快照里也记了规则。
	withSnapshot := f.insertUsage(reconUsageSpec{accountID: account, requestID: "req-bug1-snapshot", actualCost: 1, at: f.at(1)})
	f.insertExtra(withSnapshot, account, "a6", f.uniq("tok-bug1"), 1)

	// 变体二：快照 provider 为空，只有「当前规则」配好了 —— 旧实现正是在这个形状上误标。
	emptySnapshot := f.insertUsage(reconUsageSpec{accountID: account, requestID: "req-bug1-empty-snapshot", actualCost: 2, at: f.at(2)})
	f.insertExtra(emptySnapshot, account, "", "", 2)

	rows, total, err := f.ledgerRepo().Rows(f.ctx, f.from, f.to, "", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, rows, 2)

	downstream, orphans := splitLedgerRows(t, rows)
	require.Empty(t, orphans)

	for _, requestID := range []string{"req-bug1-snapshot", "req-bug1-empty-snapshot"} {
		row, ok := downstream[requestID]
		require.True(t, ok, "%s 必须出现", requestID)
		require.Equalf(t, service.ReconciliationCostSourceA6Waiting, row.CostSource,
			"%s：规则已配置、账单未到，只能是 a6_waiting", requestID)
		require.NotEqualf(t, service.ReconciliationCostSourceRuleUnconfigured, row.CostSource,
			"%s：不得误标为 rule_unconfigured（线上 1768 条误标的根因）", requestID)
	}

	// 边界：规则存在但被禁用，快照里也没有 provider —— 这才是「没有启用的规则」。
	disabledAccount := f.newAccount("recon-bug1-disabled")
	f.setRule(disabledAccount, "a6", f.uniq("tok-bug1-disabled"), false)
	disabledLog := f.insertUsage(reconUsageSpec{accountID: disabledAccount, requestID: "req-bug1-disabled", actualCost: 3, at: f.at(3)})
	f.insertExtra(disabledLog, disabledAccount, "", "", 3)

	rows, _, err = f.ledgerRepo().Rows(f.ctx, f.from, f.to, "", 1, 100)
	require.NoError(t, err)
	downstream, _ = splitLedgerRows(t, rows)
	require.Equal(t, service.ReconciliationCostSourceRuleUnconfigured, downstream["req-bug1-disabled"].CostSource,
		"规则被禁用等同于没有启用的规则")
}

// ==================== C. Bug 2 回归：令牌改名后历史账单仍能归类 ====================
//
// 分类用的令牌名必须「快照优先、当前规则兜底」：管理员把上游令牌改名后，
// 历史账单仍带旧名，只有读快照里的旧名才匹配得上（旧实现只读当前映射，实测影响 1157 条）。
func TestReconciliationLedgerRows_Bug2_TokenRenameStillMatchesHistoricalSnapshotKey(t *testing.T) {
	f := newReconFixture(t)
	account := f.newAccount("recon-bug2")

	const oldTokenName = "旧令牌名"
	const newTokenName = "新令牌名"

	f.setRule(account, "a6", oldTokenName, true)

	logID := f.insertUsage(reconUsageSpec{accountID: account, requestID: "req-bug2", actualCost: 8, at: f.at(1)})
	// 调用发生时该令牌还叫旧名，快照冻结的就是旧名。
	f.insertExtra(logID, account, "a6", oldTokenName, 8)

	// 管理员改名：当前规则换成新名，历史快照仍是旧名。
	f.renameRuleKey(account, newTokenName)

	// 窗口内的账单仍带旧名，且没有匹配到任何调用。
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-bug2"), occurredAt: f.at(2),
		tokenName: oldTokenName, costOriginal: 3, fxRateToCNY: 1, costCNY: 3,
	})

	rows, total, err := f.ledgerRepo().Rows(f.ctx, f.from, f.to, "", 1, 100)
	require.NoError(t, err)
	// 若改名把匹配打断，这张账单会掉成孤儿行，总数就不是 1 了。
	require.EqualValues(t, 1, total, "改名后不得多出一条孤儿账单")
	require.Len(t, rows, 1)

	require.Equal(t, service.ReconciliationRecordTypeDownstream, rows[0].RecordType)
	require.Equal(t, "req-bug2", rows[0].RequestID)
	require.Equal(t, service.ReconciliationCostSourceA6Pending, rows[0].CostSource,
		"按快照里的旧令牌名取值，这笔调用仍应看到「已有账单待匹配」")
	require.False(t, rows[0].Matched)
}

// ==================== D. 唯一部分索引：一条调用不能被两张账单匹配 ====================
//
// idx_reconciliation_upstream_bills_usage_key UNIQUE(matched_usage_log_id)
//
//	WHERE matched_usage_log_id IS NOT NULL
//
// 这是「一笔下游调用最多挂一笔上游账单」的硬保证，不允许绕过。
func TestReconciliationUpstreamBillRepo_UniqueUsageIndexRejectsSecondBill(t *testing.T) {
	f := newReconFixture(t)
	account := f.newAccount("recon-uniq")

	logID := f.insertUsage(reconUsageSpec{accountID: account, requestID: "req-uniq", actualCost: 1, at: f.at(1)})
	firstBill := f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-uniq-1"), occurredAt: f.at(1), costCNY: 1})
	secondBill := f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-uniq-2"), occurredAt: f.at(2), costCNY: 2})

	repo := f.billRepo()
	require.NoError(t, repo.MarkMatched(f.ctx, firstBill, logID, account, "direct"))

	// 第二张账单是 staging、matched_usage_log_id 为 NULL，MarkMatched 的 WHERE 拦不住它，
	// 能拦住它的只有唯一部分索引本身。
	err := repo.MarkMatched(f.ctx, secondBill, logID, account, "direct")
	require.Error(t, err, "同一张调用不能被两张账单匹配")
	require.Contains(t, err.Error(), "idx_reconciliation_upstream_bills_usage_key",
		"必须是唯一部分索引把它挡下来，而不是别的原因")

	// 直接写 SQL 也必须被拒（证明约束在索引上，不只在仓库方法的 WHERE 上）。
	_, err = f.db.ExecContext(f.ctx,
		`UPDATE reconciliation_upstream_bills SET matched_usage_log_id = $2 WHERE id = $1`,
		secondBill, logID)
	require.Error(t, err, "唯一部分索引本身必须拦住第二次绑定")
	require.Contains(t, err.Error(), "idx_reconciliation_upstream_bills_usage_key")

	// 第一张仍然稳稳指回这条调用；第二张没被写脏。
	firstState, firstMethod, firstMatched := f.billState(firstBill)
	require.Equal(t, "matched", firstState)
	require.Equal(t, "direct", firstMethod)
	require.EqualValues(t, logID, firstMatched)

	secondState, secondMethod, secondMatched := f.billState(secondBill)
	require.Equal(t, "staging", secondState)
	require.Empty(t, secondMethod)
	require.Zero(t, secondMatched)
}

// ==================== E. 账单导入幂等且汇率冻结 ====================
//
// ⚠️ 已知实现缺陷（在本用例中被如实暴露，未作任何绕行）：
//
//	reconciliation_upstream_bill_repo.go 的 reconciliationBillInsertColumns 是 17 列
//	（provider … source, raw），而 insertChunk 的 VALUES 模板是
//	「17 个 $n + now()」= 18 个表达式 —— 那个 now() 原本是给 imported_at 用的，
//	但 imported_at 已被刻意移出列清单（列上已有 DEFAULT now()），模板里的 now() 没跟着删。
//	结果：任何非空批次的导入都会在 PostgreSQL 侧直接失败
//	  pq: INSERT has more expressions than target columns
//	影响面：ReconciliationSyncService.SyncBills（周期同步）与 reconciliation_ops.go 的
//	手工导入都会拿到 error，上游账单永远落不了库，「已对账 / billed」状态不可达。
//	最小修法（仅记录，不由本测试实施）：删掉模板里多余的 `,now()`；顺带把
//	reconciliationBillInsertChunk 上「每行 16 个占位符」的过期注释改成 17。
func TestReconciliationUpstreamBillRepo_UpsertBatchIsIdempotentAndFreezesFx(t *testing.T) {
	f := newReconFixture(t)
	repo := f.billRepo()

	requestID := f.uniq("up-frozen")
	first := service.ReconciliationUpstreamBillPayload{
		Provider:            "a6",
		UpstreamRequestID:   requestID,
		OccurredAt:          f.at(10),
		Model:               "claude-sonnet-4-5",
		TokenName:           f.uniq("tok-frozen"),
		InputTokens:         100,
		OutputTokens:        200,
		CacheReadTokens:     10,
		CacheCreationTokens: 20,
		CacheTokensTotal:    30,
		CostOriginal:        1.25,
		Currency:            "USD",
		FxRateToCNY:         7.1,
		CostCNY:             8.875,
	}

	inserted, err := repo.UpsertBatch(f.ctx, []service.ReconciliationUpstreamBillPayload{first})
	require.NoError(t, err, "UpsertBatch 必须能导入账单；当前实现在此 100% 失败，根因见本用例上方「已知实现缺陷」")
	require.EqualValues(t, 1, inserted, "首次导入应新增 1 条")

	// 第二次导入同一条账单（同 provider + upstream_request_id），
	// 连同成本、汇率、甚至元信息一起改掉。
	second := first
	second.CostOriginal = 99
	second.FxRateToCNY = 8.5
	second.CostCNY = 841.5
	second.Model = "claude-opus-4-1"

	inserted, err = repo.UpsertBatch(f.ctx, []service.ReconciliationUpstreamBillPayload{second})
	require.NoError(t, err)
	require.EqualValues(t, 0, inserted, "重复导入不得新增条数")

	// DO NOTHING 整行跳过：成本与汇率在首次导入时冻结，元信息也不被覆盖。
	var gotOriginal, gotFxRate, gotCNY float64
	var gotModel string
	require.NoError(t, scanSingleRow(f.ctx, f.db, `
SELECT cost_original, fx_rate_to_cny, cost_cny, model
FROM reconciliation_upstream_bills
WHERE provider = $1 AND upstream_request_id = $2`,
		[]any{"a6", requestID},
		&gotOriginal, &gotFxRate, &gotCNY, &gotModel))

	require.InDelta(t, 1.25, gotOriginal, 1e-9, "cost_original 必须保持首次导入的值")
	require.InDelta(t, 7.1, gotFxRate, 1e-9, "fx_rate_to_cny 必须保持首次导入的值")
	require.InDelta(t, 8.875, gotCNY, 1e-9, "cost_cny 必须保持首次导入的值")
	require.Equal(t, "claude-sonnet-4-5", gotModel)

	var count int64
	require.NoError(t, scanSingleRow(f.ctx, f.db,
		`SELECT COUNT(*) FROM reconciliation_upstream_bills WHERE provider = $1 AND occurred_at >= $2 AND occurred_at < $3`,
		[]any{"a6", f.from, f.to}, &count))
	require.EqualValues(t, 1, count)

	// 空批次是 no-op。
	inserted, err = repo.UpsertBatch(f.ctx, nil)
	require.NoError(t, err)
	require.Zero(t, inserted)
}

// ==================== F. Summary 口径：上游成本只算已匹配的账单 ====================
func TestReconciliationLedgerSummary_UpstreamCostCountsMatchedBillsOnly(t *testing.T) {
	f := newReconFixture(t)

	matchedAccount := f.newAccount("recon-summary-matched")
	waitingAccount := f.newAccount("recon-summary-waiting")

	// 下游调用一：收入 10，匹配到一张 cost_cny=4 的账单。
	matchedLog := f.insertUsage(reconUsageSpec{accountID: matchedAccount, requestID: "req-summary-matched", actualCost: 10, at: f.at(1)})
	f.insertExtra(matchedLog, matchedAccount, "a6", f.uniq("tok-summary"), 10)
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-summary-matched"), occurredAt: f.at(1), tokenName: f.uniq("tok-summary"),
		costOriginal: 4, fxRateToCNY: 1, costCNY: 4,
		matchState: "matched", matchMethod: "direct",
		matchedUsageLogID: matchedLog, matchedAccountID: matchedAccount,
	})

	// 下游调用二：收入 10，账单还没到。
	waitingLog := f.insertUsage(reconUsageSpec{accountID: waitingAccount, requestID: "req-summary-waiting", actualCost: 10, at: f.at(2)})
	f.setRule(waitingAccount, "a6", f.uniq("tok-summary-waiting"), true)
	f.insertExtra(waitingLog, waitingAccount, "a6", f.uniq("tok-summary-waiting"), 10)

	// 孤儿账单：有 7 的成本，却没有对应的下游收入。
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-summary-orphan"), occurredAt: f.at(3),
		tokenName: f.uniq("tok-summary-orphan"), costOriginal: 7, fxRateToCNY: 1, costCNY: 7,
		matchState: "unmatched",
	})

	summary, err := f.ledgerRepo().Summary(f.ctx, f.from, f.to)
	require.NoError(t, err)

	require.InDelta(t, 20, summary.RevenueCNY, 1e-9, "收入只统计下游调用")
	require.InDelta(t, 10, summary.MatchedRevenueCNY, 1e-9)
	require.InDelta(t, 4, summary.UpstreamCostCNY, 1e-9, "上游成本只算已匹配账单，孤儿账单的 7 不得计入")
	require.EqualValues(t, 1, summary.Matched)
	require.EqualValues(t, 1, summary.Unmatched)
	require.EqualValues(t, 1, summary.UpstreamUnmatched)
	require.EqualValues(t, 2, summary.BilledCount, "窗口内账单总数为 2（已匹配 1 + 孤儿 1）")
	require.Equal(t, f.from, summary.From)
	require.Equal(t, f.to, summary.To)
}

// ==================== G. MarkMatched / MarkUnmatched 的状态守卫 ====================
func TestReconciliationUpstreamBillRepo_MarkMatchedAndMarkUnmatchedStateGuards(t *testing.T) {
	f := newReconFixture(t)
	account := f.newAccount("recon-guard")

	logID := f.insertUsage(reconUsageSpec{accountID: account, requestID: "req-guard-1", actualCost: 1, at: f.at(1)})
	otherLogID := f.insertUsage(reconUsageSpec{accountID: account, requestID: "req-guard-2", actualCost: 1, at: f.at(2)})

	matchedBill := f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-guard-1"), occurredAt: f.at(1), costCNY: 1})
	stagingBill := f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-guard-2"), occurredAt: f.at(2), costCNY: 2})

	repo := f.billRepo()

	require.NoError(t, repo.MarkMatched(f.ctx, matchedBill, logID, account, "direct"))
	state, method, matched := f.billState(matchedBill)
	require.Equal(t, "matched", state)
	require.Equal(t, "direct", method)
	require.EqualValues(t, logID, matched)

	// MarkUnmatched 只能作用于 staging：已匹配的账单不能被降级，
	// 否则一次失败的补匹配就会把已经对好的账抹掉。
	require.NoError(t, repo.MarkUnmatched(f.ctx, []int64{matchedBill, stagingBill}))

	state, method, matched = f.billState(matchedBill)
	require.Equal(t, "matched", state, "已匹配的账单不得被 MarkUnmatched 降级")
	require.Equal(t, "direct", method, "降级失败时也不能清掉匹配方式")
	require.EqualValues(t, logID, matched, "降级失败时也不能丢掉匹配到的调用")

	state, _, matched = f.billState(stagingBill)
	require.Equal(t, "unmatched", state, "staging 账单应被正常标记为 unmatched")
	require.Zero(t, matched)

	// 已匹配账单再次 MarkMatched（甚至换一条调用）不得改写首次匹配结果：
	// WHERE matched_usage_log_id IS NULL 让它变成 no-op，而不是撞唯一索引。
	require.NoError(t, repo.MarkMatched(f.ctx, matchedBill, otherLogID, account, "composite"))
	state, method, matched = f.billState(matchedBill)
	require.Equal(t, "matched", state)
	require.Equal(t, "direct", method, "第二次 MarkMatched 不得覆盖首次匹配方式")
	require.EqualValues(t, logID, matched, "第二次 MarkMatched 不得改写首次匹配到的调用")

	// unmatched 不是 staging，再次 MarkUnmatched 是 no-op（也不报错）。
	require.NoError(t, repo.MarkUnmatched(f.ctx, []int64{stagingBill}))
	state, _, _ = f.billState(stagingBill)
	require.Equal(t, "unmatched", state)

	// 空列表是 no-op。
	require.NoError(t, repo.MarkUnmatched(f.ctx, nil))
	require.NoError(t, repo.MarkUnmatched(f.ctx, []int64{}))
}

// ==================== 其余仓库方法 ====================

func TestReconciliationUpstreamBillRepo_ListStagingAndMatchCandidates(t *testing.T) {
	f := newReconFixture(t)
	account := f.newAccount("recon-candidates")
	otherAccount := f.newAccount("recon-candidates-other")

	// ListStaging 只取 match_state='staging'，按 occurred_at 升序。
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-stage-2"), occurredAt: f.at(2), costCNY: 1})
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-stage-1"), occurredAt: f.at(1), costCNY: 1})
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-stage-unmatched"), occurredAt: f.at(3), costCNY: 1, matchState: "unmatched"})
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-stage-matched"), occurredAt: f.at(4), costCNY: 1, matchState: "matched"})

	repo := f.billRepo()

	staging, err := repo.ListStaging(f.ctx, f.from, f.to, 10)
	require.NoError(t, err)
	require.Len(t, staging, 2)
	require.Equal(t, f.uniq("up-stage-1"), staging[0].Payload.UpstreamRequestID)
	require.Equal(t, f.uniq("up-stage-2"), staging[1].Payload.UpstreamRequestID)
	require.Equal(t, "a6", staging[0].Payload.Provider)
	require.Equal(t, "USD", staging[0].Payload.Currency)
	require.InDelta(t, 1, staging[0].Payload.CostCNY, 1e-9)
	require.Nil(t, staging[0].Payload.BillingDate, "billing_date 为空时必须是 nil")

	limited, err := repo.ListStaging(f.ctx, f.from, f.to, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1, "limit 必须生效")

	// limit <= 0 回落为默认上限，不能变成「一条都不返回」。
	def, err := repo.ListStaging(f.ctx, f.from, f.to, 0)
	require.NoError(t, err)
	require.Len(t, def, 2)

	// 窗口外一条都不取。
	outside, err := repo.ListStaging(f.ctx, f.to, f.to.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, outside)

	// FindDirectMatchCandidates：按上游请求 ID 找候选，已挂过账单的调用必须被排除。
	sharedUpstreamRequest := f.uniq("up-req-shared")
	freeLog := f.insertUsage(reconUsageSpec{
		accountID: account, model: "m-direct", requestID: "req-direct-free",
		upstreamRequestID: sharedUpstreamRequest, inputTokens: 7, outputTokens: 8,
		cacheReadTokens: 1, cacheCreation: 2, at: f.at(5),
	})
	boundLog := f.insertUsage(reconUsageSpec{
		accountID: account, model: "m-direct", requestID: "req-direct-bound",
		upstreamRequestID: sharedUpstreamRequest, at: f.at(6),
	})
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-direct-bound"), occurredAt: f.at(6),
		matchState: "matched", matchMethod: "direct",
		matchedUsageLogID: boundLog, matchedAccountID: account,
	})

	candidates, err := repo.FindDirectMatchCandidates(f.ctx, sharedUpstreamRequest)
	require.NoError(t, err)
	require.Len(t, candidates, 1, "已挂过账单的调用必须被排除")
	require.EqualValues(t, freeLog, candidates[0].UsageLogID)
	require.EqualValues(t, account, candidates[0].AccountID)
	require.Equal(t, "m-direct", candidates[0].Model)
	require.Equal(t, sharedUpstreamRequest, candidates[0].UpstreamRequestID)
	require.EqualValues(t, 7, candidates[0].InputTokens)
	require.EqualValues(t, 8, candidates[0].OutputTokens)
	require.EqualValues(t, 1, candidates[0].CacheReadTokens)
	require.EqualValues(t, 2, candidates[0].CacheCreationTok)

	blank, err := repo.FindDirectMatchCandidates(f.ctx, "   ")
	require.NoError(t, err)
	require.Empty(t, blank, "空白的上游请求 ID 不应查出任何候选")

	// FindCompositeMatchCandidates：只做账号 + 模型 + 输出 token + 时间窗口的粗筛。
	compositeModel := f.uniq("m-comp")
	occurredAt := f.at(30)
	expected := f.insertUsage(reconUsageSpec{
		accountID: account, model: compositeModel, requestID: "req-comp-ok",
		outputTokens: 100, inputTokens: 10, cacheReadTokens: 5, cacheCreation: 5, at: occurredAt,
	})
	f.insertUsage(reconUsageSpec{accountID: account, model: compositeModel, requestID: "req-comp-out-of-window", outputTokens: 100, at: f.at(45)})
	f.insertUsage(reconUsageSpec{accountID: account, model: f.uniq("m-comp-other"), requestID: "req-comp-model-mismatch", outputTokens: 100, at: occurredAt})
	f.insertUsage(reconUsageSpec{accountID: account, model: compositeModel, requestID: "req-comp-output-mismatch", outputTokens: 50, at: occurredAt})
	f.insertUsage(reconUsageSpec{accountID: otherAccount, model: compositeModel, requestID: "req-comp-account-mismatch", outputTokens: 100, at: occurredAt})
	compositeBound := f.insertUsage(reconUsageSpec{accountID: account, model: compositeModel, requestID: "req-comp-bound", outputTokens: 100, at: occurredAt})
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-comp-bound"), occurredAt: occurredAt,
		matchState: "matched", matchMethod: "composite",
		matchedUsageLogID: compositeBound, matchedAccountID: account,
	})

	got, err := repo.FindCompositeMatchCandidates(f.ctx, service.ReconciliationCompositeQuery{
		AccountIDs:   []int64{account},
		Model:        compositeModel,
		OutputTokens: 100,
		OccurredAt:   occurredAt,
		TimeWindow:   2 * time.Minute,
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "只应留下唯一同时满足账号/模型/输出 token/时间窗口且未挂账单的调用")
	require.EqualValues(t, expected, got[0].UsageLogID)
	require.EqualValues(t, 10, got[0].InputTokens)
	require.EqualValues(t, 5, got[0].CacheReadTokens)

	empty, err := repo.FindCompositeMatchCandidates(f.ctx, service.ReconciliationCompositeQuery{
		Model: compositeModel, OutputTokens: 100, OccurredAt: occurredAt,
	})
	require.NoError(t, err)
	require.Empty(t, empty, "AccountIDs 为空时不应返回任何候选（否则会全表扫）")
}

func TestReconciliationUpstreamBillRepo_ProviderTokenNamesAndCountUnmatched(t *testing.T) {
	f := newReconFixture(t)
	repo := f.billRepo()

	tokenA, tokenB, tokenC := f.uniq("tok-names-a"), f.uniq("tok-names-b"), f.uniq("tok-names-c")

	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-names-1"), occurredAt: f.at(1), tokenName: tokenA, costCNY: 1})
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-names-2"), occurredAt: f.at(2), tokenName: tokenA, costCNY: 1})
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-names-3"), occurredAt: f.at(3), tokenName: tokenB, costCNY: 1})
	f.insertBill(reconBillSpec{upstreamRequestID: f.uniq("up-names-4"), occurredAt: f.at(4), tokenName: tokenC, costCNY: 1, matchState: "unmatched"})

	names, err := repo.ProviderTokenNames(f.ctx, f.from, f.to)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{tokenA: 2, tokenB: 1, tokenC: 1}, names)

	unmatched, err := repo.CountUnmatched(f.ctx, f.from, f.to)
	require.NoError(t, err)
	require.EqualValues(t, 1, unmatched)

	outsideWindow, err := repo.CountUnmatched(f.ctx, f.to, f.to.Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, outsideWindow)

	outsideNames, err := repo.ProviderTokenNames(f.ctx, f.to, f.to.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, outsideNames)
}

func TestReconciliationUsageExtraRepo_UpsertBatchIsIdempotentAndLookupsWork(t *testing.T) {
	f := newReconFixture(t)
	repo := f.extraRepo()

	accountA := f.newAccount("recon-extra-a")
	accountB := f.newAccount("recon-extra-b")

	logA := f.insertUsage(reconUsageSpec{accountID: accountA, requestID: "req-extra-a", actualCost: 1, at: f.at(1)})
	logB := f.insertUsage(reconUsageSpec{accountID: accountB, requestID: "req-extra-b", actualCost: 2, at: f.at(2)})
	logC := f.insertUsage(reconUsageSpec{accountID: accountB, requestID: "req-extra-c", actualCost: 3, at: f.at(3)})

	keyA, keyB := f.uniq("key-a"), f.uniq("key-b")

	inserted, err := repo.UpsertBatch(f.ctx, []service.ReconciliationUsageExtra{
		{UsageLogID: logA, AccountID: accountA, RuleProvider: "a6", RuleExternalKey: keyA, RuleVersion: 1, RevenueOriginal: 1, FxRateToCNY: 7, RevenueCNY: 7, CollectedAt: f.at(1)},
		{UsageLogID: logB, AccountID: accountB, RuleProvider: "a6", RuleExternalKey: keyB, RuleVersion: 1, RevenueOriginal: 2, FxRateToCNY: 7, RevenueCNY: 14, CollectedAt: f.at(2)},
		// 同一批里重复出现同一条调用：按 usage_log_id 去重，新增条数不得虚高。
		{UsageLogID: logB, AccountID: accountB, RuleProvider: "a6", RuleExternalKey: keyB, RuleVersion: 1, RevenueOriginal: 9, FxRateToCNY: 7, RevenueCNY: 63},
		// usage_log_id 是快照的主键语义，<=0 的行直接跳过。
		{UsageLogID: 0, AccountID: accountA},
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, inserted)

	// 重放：快照一旦落库即冻结，不得覆盖已落库的收入与汇率。
	inserted, err = repo.UpsertBatch(f.ctx, []service.ReconciliationUsageExtra{
		{UsageLogID: logA, AccountID: accountA, RuleProvider: "a6", RuleExternalKey: keyA, RevenueOriginal: 100, FxRateToCNY: 9, RevenueCNY: 900},
	})
	require.NoError(t, err)
	require.Zero(t, inserted)

	var revenueCNY, fxRate float64
	require.NoError(t, scanSingleRow(f.ctx, f.db,
		`SELECT revenue_cny, fx_rate_to_cny FROM reconciliation_usage_extras WHERE usage_log_id = $1`,
		[]any{logA}, &revenueCNY, &fxRate))
	require.InDelta(t, 7, revenueCNY, 1e-9, "重放不得覆盖已冻结的收入")
	require.InDelta(t, 7, fxRate, 1e-9, "重放不得覆盖已冻结的汇率")

	// 空批次与全无效批次都是 no-op。
	inserted, err = repo.UpsertBatch(f.ctx, nil)
	require.NoError(t, err)
	require.Zero(t, inserted)

	// ListCollectedUsageLogIDs：只有采过的 ID 出现在返回值里。
	collected, err := repo.ListCollectedUsageLogIDs(f.ctx, []int64{logA, logB, logC})
	require.NoError(t, err)
	require.Len(t, collected, 2)
	require.Contains(t, collected, logA)
	require.Contains(t, collected, logB)
	require.NotContains(t, collected, logC, "没采过的 ID 不能出现在结果里")

	emptyCollected, err := repo.ListCollectedUsageLogIDs(f.ctx, nil)
	require.NoError(t, err)
	require.Empty(t, emptyCollected)

	// ListAccountIDsByRuleKeys：令牌改名后按历史快照名反查账号，按账号 ID 升序去重。
	accountIDs, err := repo.ListAccountIDsByRuleKeys(f.ctx, []string{keyA, keyB, "", "   ", keyA})
	require.NoError(t, err)
	require.Equal(t, []int64{accountA, accountB}, accountIDs)

	onlyUnknown, err := repo.ListAccountIDsByRuleKeys(f.ctx, []string{f.uniq("key-missing")})
	require.NoError(t, err)
	require.Empty(t, onlyUnknown)

	blankKeys, err := repo.ListAccountIDsByRuleKeys(f.ctx, []string{"", "  "})
	require.NoError(t, err)
	require.Equal(t, []int64{}, blankKeys, "空串不参与反查，且必须返回空切片而不是 nil")
}

func TestReconciliationLedgerRepo_PointsAndUsageCountsByAccount(t *testing.T) {
	f := newReconFixture(t)
	accountA := f.newAccount("recon-usage-a")
	accountB := f.newAccount("recon-usage-b")

	matchedLog := f.insertUsage(reconUsageSpec{
		accountID: accountA, model: "m-points-1", requestID: "req-points-1", actualCost: 1.5, at: f.at(10),
	})
	f.insertExtra(matchedLog, accountA, "a6", f.uniq("tok-points-1"), 1.5)
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-points-1"), occurredAt: f.at(10), tokenName: f.uniq("tok-points-1"),
		costOriginal: 3, fxRateToCNY: 1, costCNY: 3,
		matchState: "matched", matchMethod: "direct",
		matchedUsageLogID: matchedLog, matchedAccountID: accountA,
	})

	waitingLog := f.insertUsage(reconUsageSpec{
		accountID: accountA, model: "m-points-2", requestID: "req-points-2", actualCost: 2.5, at: f.at(20),
	})
	f.insertExtra(waitingLog, accountA, "a6", f.uniq("tok-points-2"), 2.5)

	f.insertUsage(reconUsageSpec{
		accountID: accountB, model: "m-points-3", requestID: "req-points-3", actualCost: 0, at: f.at(30),
	})

	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-points-orphan"), occurredAt: f.at(40),
		tokenName: f.uniq("tok-points-orphan"), costOriginal: 5, fxRateToCNY: 1, costCNY: 5,
		matchState: "unmatched",
	})

	// Points：整个窗口落在同一个整点小时桶里。
	points, err := f.ledgerRepo().Points(f.ctx, f.from, f.to, time.Hour)
	require.NoError(t, err)
	require.Len(t, points, 1)

	point := points[0]
	require.True(t, point.Start.Equal(f.from), "桶起点应回到整点 %s，实际 %s", f.from, point.Start)
	require.InDelta(t, 4, point.RevenueCNY, 1e-9)
	require.InDelta(t, 3, point.UpstreamCostCNY, 1e-9, "只有已匹配账单的成本进趋势")
	require.EqualValues(t, 1, point.Matched)
	require.EqualValues(t, 2, point.Unmatched)
	require.EqualValues(t, 1, point.UpstreamUnmatched)

	// bucket <= 0 回落为一小时，不应报错也不应返回空。
	fallback, err := f.ledgerRepo().Points(f.ctx, f.from, f.to, 0)
	require.NoError(t, err)
	require.Len(t, fallback, 1)
	require.True(t, fallback[0].Start.Equal(f.from))

	// UsageCountsByAccount：窗口内各账号的调用数、首末调用时间与模型名。
	usage, err := f.ledgerRepo().UsageCountsByAccount(f.ctx, f.from, f.to)
	require.NoError(t, err)
	require.Len(t, usage, 2)

	require.EqualValues(t, 2, usage[accountA].Count)
	require.EqualValues(t, 1, usage[accountB].Count)
	require.ElementsMatch(t, []string{"m-points-1", "m-points-2"}, usage[accountA].Models)
	require.ElementsMatch(t, []string{"m-points-3"}, usage[accountB].Models)
	require.True(t, usage[accountA].FirstSeen.Equal(f.at(10)))
	require.True(t, usage[accountA].LastSeen.Equal(f.at(20)))
	require.EqualValues(t, accountA, usage[accountA].AccountID)

	outside, err := f.ledgerRepo().UsageCountsByAccount(f.ctx, f.to, f.to.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, outside)
}

func TestReconciliationLedgerRepo_RowsStatusFiltersAndPaging(t *testing.T) {
	f := newReconFixture(t)
	account := f.newAccount("recon-rows-filter")
	matchedToken := f.uniq("tok-rows-matched")

	matchedLog := f.insertUsage(reconUsageSpec{
		accountID: account, model: "m-rows", requestID: "req-rows-matched", actualCost: 1, at: f.at(1),
	})
	f.insertExtra(matchedLog, account, "a6", matchedToken, 1)
	f.insertBill(reconBillSpec{
		upstreamRequestID: f.uniq("up-rows-matched"), occurredAt: f.at(1), tokenName: matchedToken,
		costOriginal: 2, fxRateToCNY: 1, costCNY: 2,
		matchState: "matched", matchMethod: "direct",
		matchedUsageLogID: matchedLog, matchedAccountID: account,
	})

	unmatchedLog := f.insertUsage(reconUsageSpec{
		accountID: account, model: "m-rows", requestID: "req-rows-unmatched", actualCost: 1, at: f.at(2),
	})
	f.insertExtra(unmatchedLog, account, "a6", f.uniq("tok-rows-unmatched"), 1)

	orphanRequestID := f.uniq("up-rows-orphan")
	f.insertBill(reconBillSpec{
		upstreamRequestID: orphanRequestID, occurredAt: f.at(3),
		tokenName: f.uniq("tok-rows-orphan"), costOriginal: 9, fxRateToCNY: 1, costCNY: 9,
		matchState: "unmatched",
	})

	repo := f.ledgerRepo()

	// status 为空 = 全部，按 created_at DESC, source_id DESC 排序。
	all, total, err := repo.Rows(f.ctx, f.from, f.to, "", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	require.Len(t, all, 3)
	require.Equal(t, service.ReconciliationRecordTypeUpstreamUnmatched, all[0].RecordType, "最新的孤儿账单排在最前")
	require.Equal(t, "req-rows-unmatched", all[1].RequestID)
	require.Equal(t, "req-rows-matched", all[2].RequestID)

	// status=matched
	matchedRows, matchedTotal, err := repo.Rows(f.ctx, f.from, f.to, "matched", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, matchedTotal)
	require.Len(t, matchedRows, 1)
	require.Equal(t, service.ReconciliationCostSourceBilled, matchedRows[0].CostSource)
	require.True(t, matchedRows[0].Matched)
	require.True(t, matchedRows[0].HasUpstreamCost)
	require.InDelta(t, 2, matchedRows[0].UpstreamCostCNY, 1e-9)
	require.InDelta(t, 2, matchedRows[0].UpstreamCostOrig, 1e-9)
	require.InDelta(t, 1, matchedRows[0].UpstreamFxRateCNY, 1e-9)
	require.Equal(t, "USD", matchedRows[0].UpstreamCurrency)
	require.Equal(t, "direct", matchedRows[0].UpstreamMatchMethod)
	require.EqualValues(t, account, matchedRows[0].AccountID)
	require.EqualValues(t, f.userID, matchedRows[0].UserID)
	require.EqualValues(t, f.groupID, matchedRows[0].GroupID)
	require.NotEmpty(t, matchedRows[0].UserEmail, "user_email 来自 users 的 LEFT JOIN")

	// status=unmatched：只数「下游未对账的调用」。孤儿账单走 upstream_unmatched，
	// 不能混进这个口径，否则一笔孤儿成本会被当成两笔未对账。
	unmatchedRows, unmatchedTotal, err := repo.Rows(f.ctx, f.from, f.to, "unmatched", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, unmatchedTotal, "本窗口只有 1 条下游调用未对账（孤儿账单不算）")
	require.Len(t, unmatchedRows, 1)
	require.Equal(t, "req-rows-unmatched", unmatchedRows[0].RequestID)
	for _, row := range unmatchedRows {
		require.Equal(t, service.ReconciliationRecordTypeDownstream, row.RecordType)
		require.False(t, row.Matched)
		require.False(t, row.HasUpstreamCost, "未对账的调用必须给出「成本未知」，接口层据此输出空串让前端显示 —")
		require.Zero(t, row.UpstreamCostCNY)
		require.Empty(t, row.UpstreamCurrency)
		require.Empty(t, row.UpstreamMatchMethod)
	}

	// status=upstream_unmatched
	orphanRows, orphanTotal, err := repo.Rows(f.ctx, f.from, f.to, "upstream_unmatched", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, orphanTotal)
	require.Len(t, orphanRows, 1)
	require.Equal(t, orphanRequestID, orphanRows[0].UpstreamRequestID)
	require.Zero(t, orphanRows[0].SourceID)
	require.True(t, orphanRows[0].HasUpstreamCost)
	require.InDelta(t, 9, orphanRows[0].UpstreamCostCNY, 1e-9)

	// 未知 status 退回「全部」，不报错也不返回空。
	unknown, unknownTotal, err := repo.Rows(f.ctx, f.from, f.to, "no-such-status", 1, 100)
	require.NoError(t, err)
	require.EqualValues(t, 3, unknownTotal)
	require.Len(t, unknown, 3)

	// 分页：pageSize=1 逐页取回，两页不重复。
	page1, pageTotal, err := repo.Rows(f.ctx, f.from, f.to, "", 1, 1)
	require.NoError(t, err)
	require.EqualValues(t, 3, pageTotal, "命中总数与分页无关")
	require.Len(t, page1, 1)

	page2, _, err := repo.Rows(f.ctx, f.from, f.to, "", 2, 1)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.NotEqual(t, page1[0].SourceID, page2[0].SourceID)

	// pageSize 上限 100：要求 1000 只会被夹到 100，不会报错。
	clamped, _, err := repo.Rows(f.ctx, f.from, f.to, "", 1, 1000)
	require.NoError(t, err)
	require.Len(t, clamped, 3)
}
