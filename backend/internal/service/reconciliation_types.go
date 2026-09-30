package service

import (
	"context"
	"time"
)

// ==================== 对账状态 ====================

// ReconciliationCostSource 是看板明细行上的对账状态。
//
// 取值必须与前端 CompanionRequestsTable.vue 的 STATUS_SUFFIX 映射表对齐，
// 前端按这些值查 i18n 文案；未命中时才回落到响应里的 cost_source_label。
//
// 判定顺序即优先级，见 ClassifyDownstreamCostSource。
type ReconciliationCostSource string

const (
	// ReconciliationCostSourceBilled 已匹配到上游账单，成本确定。
	ReconciliationCostSourceBilled ReconciliationCostSource = "billed"
	// ReconciliationCostSourceA6Waiting 该账号已配置规则，但对应令牌还没有任何上游账单。
	ReconciliationCostSourceA6Waiting ReconciliationCostSource = "a6_waiting"
	// ReconciliationCostSourceA6Pending 该账号已配置规则，对应令牌已有账单，但这一笔没匹配上。
	ReconciliationCostSourceA6Pending ReconciliationCostSource = "a6_pending"
	// ReconciliationCostSourceRuleUnconfigured 没有规则。
	//
	// 注意：只有当规则快照为空「且」该账号当前也没有规则时才是这个状态。
	// 旧实现用「provider 不等于 subarx」兜底，把「已配置但账单未到」误标成本状态，
	// 线上一次误标 1768 条，本实现必须避免。
	ReconciliationCostSourceRuleUnconfigured ReconciliationCostSource = "rule_unconfigured"
	// ReconciliationCostSourceUpstreamUnmatched 上游账单没有对应的本站调用（孤儿账单行专用）。
	ReconciliationCostSourceUpstreamUnmatched ReconciliationCostSource = "upstream_unmatched"
	// ReconciliationCostSourcePending 兜底状态。
	ReconciliationCostSourcePending ReconciliationCostSource = "pending"
)

// ReconciliationCostSourceLabel 返回状态的中文标签。
//
// 前端对已知取值走 i18n，本函数只在取值未知时作为回落文案；
// 因此返回值必须非空，否则看板上会出现空徽标。
func ReconciliationCostSourceLabel(source ReconciliationCostSource) string {
	switch source {
	case ReconciliationCostSourceBilled:
		return "账单实扣"
	case ReconciliationCostSourceA6Waiting:
		return "等待上游账单"
	case ReconciliationCostSourceA6Pending:
		return "上游账单待匹配"
	case ReconciliationCostSourceRuleUnconfigured:
		return "规则待配置"
	case ReconciliationCostSourceUpstreamUnmatched:
		return "上游待匹配"
	case ReconciliationCostSourcePending:
		return "待对账"
	default:
		return "待对账"
	}
}

// ==================== 明细与统计 ====================

// ReconciliationRecordType 区分明细行的来源。
const (
	// ReconciliationRecordTypeDownstream 本站调用行。
	ReconciliationRecordTypeDownstream = "downstream"
	// ReconciliationRecordTypeUpstreamUnmatched 孤儿的上游账单行。
	ReconciliationRecordTypeUpstreamUnmatched = "upstream_unmatched"
)

// ReconciliationLedgerRow 是明细列表与汇总统计共用的原始事实行。
//
// 汇总与明细必须由同一份判定逻辑产出，否则会出现「顶部说 10 笔未对账、
// 列表只列出 8 笔」这类自相矛盾的展示。旧实现一处走 SQL 条件、一处走 Go 分支，
// 是手工镜像的两个副本，本实现统一为一次分类结果。
type ReconciliationLedgerRow struct {
	RecordType string
	SourceID   int64
	CreatedAt  time.Time

	RequestID         string
	UpstreamRequestID string

	UserID    int64
	UserEmail string
	APIKeyID  int64
	AccountID int64
	GroupID   int64
	GroupName string

	Model        string
	InputTokens  int
	OutputTokens int
	CacheTokens  int

	// RevenueCNY 下游收入（已按采集时汇率冻结）。
	RevenueCNY float64

	// 上游成本；HasUpstreamCost 为 false 时表示还没对上账单，
	// 接口层必须输出空串而不是 0，前端据此显示「—」。
	HasUpstreamCost     bool
	UpstreamCostCNY     float64
	UpstreamCostOrig    float64
	UpstreamCurrency    string
	UpstreamFxRateCNY   float64
	UpstreamMatchMethod string

	CostSource ReconciliationCostSource
	Matched    bool
}

// ReconciliationGrossProfitCNY 返回该行毛利；只有已对账的行才有毛利。
//
// 孤儿账单行虽然有成本，却没有对应的下游收入，算毛利会得到一个负的成本数字，
// 误导性很强。因此这里与 profit_scope = matched_only 保持一致，只对已对账行计算。
func (r ReconciliationLedgerRow) ReconciliationGrossProfitCNY() (float64, bool) {
	if !r.Matched {
		return 0, false
	}
	return r.RevenueCNY - r.UpstreamCostCNY, true
}

// ReconciliationSummary 是汇总指标。
type ReconciliationSummary struct {
	From time.Time
	To   time.Time

	RevenueCNY        float64
	MatchedRevenueCNY float64
	UpstreamCostCNY   float64

	Matched           int64
	Unmatched         int64
	UpstreamUnmatched int64
	BilledCount       int64
}

// ReconciliationBucketPoint 是趋势图的一个分桶。
type ReconciliationBucketPoint struct {
	Start             time.Time
	RevenueCNY        float64
	UpstreamCostCNY   float64
	Matched           int64
	Unmatched         int64
	UpstreamUnmatched int64
}

// ==================== 账号规则 ====================

// ReconciliationAccountRule 是一条账号规则。
type ReconciliationAccountRule struct {
	ID          int64
	AccountID   int64
	Provider    string
	ExternalKey string
	Multiplier  *float64
	Version     int64
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ==================== 上游账单 ====================

// ReconciliationUpstreamBill 是一条上游逐笔账单。
type ReconciliationUpstreamBill struct {
	ID      int64
	Payload ReconciliationUpstreamBillPayload
}

// ReconciliationUpstreamBillPayload 是写入上游账单时需要的全部字段。
//
// 成本与汇率在导入时冻结：重复导入同一条账单不得覆盖 cost_original /
// fx_rate_to_cny / cost_cny，否则历史金额会随汇率调整而漂移。
type ReconciliationUpstreamBillPayload struct {
	Provider            string
	UpstreamRequestID   string
	OccurredAt          time.Time
	BillingDate         *time.Time
	Model               string
	TokenName           string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	// CacheTokensTotal 是上游口径的缓存合计。
	//
	// 上游可能只回一个合并值而不分读写，此时上面两个分列字段为 0、本字段保留合并值。
	// 组合匹配一律以本字段为准：若拿分列的 0 去比对，只回合并值的账单会全部匹配失败。
	CacheTokensTotal int
	CostOriginal     float64
	Currency         string
	FxRateToCNY      float64
	CostCNY          float64
	Source           string
	Raw              map[string]any
}

// ReconciliationUsageExtra 是调用侧扩展快照。
type ReconciliationUsageExtra struct {
	UsageLogID      int64
	AccountID       int64
	RuleProvider    string
	RuleExternalKey string
	RuleVersion     int64
	RevenueOriginal float64
	FxRateToCNY     float64
	RevenueCNY      float64
	CollectedAt     time.Time
}

// ==================== 端口接口 ====================
//
// 按仓库约定，repository 侧的接口声明在 service 包，由 repository 包实现，
// 且 repository 的构造函数返回这些接口本身。

// ReconciliationUsageExtraRepository 调用侧快照的读写。
type ReconciliationUsageExtraRepository interface {
	// UpsertBatch 幂等写入快照；已存在的行不覆盖（快照一旦落库即冻结）。
	UpsertBatch(ctx context.Context, extras []ReconciliationUsageExtra) (int64, error)
	// ListCollectedUsageLogIDs 返回给定 ID 集合中已经采过的部分，用于跳过重复采集。
	ListCollectedUsageLogIDs(ctx context.Context, usageLogIDs []int64) (map[int64]struct{}, error)
	// ListAccountIDsByRuleKeys 反查历史上使用过这些令牌名的账号，
	// 用于令牌改名后仍能匹配旧账单。
	ListAccountIDsByRuleKeys(ctx context.Context, ruleKeys []string) ([]int64, error)
}

// ReconciliationUpstreamBillRepository 上游账单的读写与匹配。
type ReconciliationUpstreamBillRepository interface {
	// UpsertBatch 幂等导入账单，返回实际新增条数。
	// 冲突时只更新匹配相关字段之外的元信息，绝不覆盖成本与汇率。
	UpsertBatch(ctx context.Context, bills []ReconciliationUpstreamBillPayload) (int64, error)
	// ListStaging 取出尚未完成首次匹配的账单。
	ListStaging(ctx context.Context, from, to time.Time, limit int) ([]ReconciliationUpstreamBill, error)
	// FindDirectMatchCandidates 按上游请求 ID 找出可匹配的下游调用。
	FindDirectMatchCandidates(ctx context.Context, upstreamRequestID string) ([]ReconciliationMatchCandidate, error)
	// FindCompositeMatchCandidates 按账号范围 + 模型 + token 数 + 时间窗口找出候选调用。
	FindCompositeMatchCandidates(ctx context.Context, query ReconciliationCompositeQuery) ([]ReconciliationMatchCandidate, error)
	// MarkMatched 把账单标记为已匹配。
	MarkMatched(ctx context.Context, billID, usageLogID, accountID int64, method string) error
	// MarkUnmatched 把账单标记为确认匹配不上。
	MarkUnmatched(ctx context.Context, billIDs []int64) error
	// ProviderTokenNames 返回窗口内出现过账单的上游令牌名及其账单数。
	ProviderTokenNames(ctx context.Context, from, to time.Time) (map[string]int64, error)
	// CountUnmatched 统计窗口内孤儿账单数。
	CountUnmatched(ctx context.Context, from, to time.Time) (int64, error)
}

// ReconciliationMatchCandidate 是一个可能匹配到上游账单的下游调用。
type ReconciliationMatchCandidate struct {
	UsageLogID        int64
	AccountID         int64
	Model             string
	UpstreamRequestID string
	InputTokens       int
	OutputTokens      int
	CacheReadTokens   int
	CacheCreationTok  int
	CreatedAt         time.Time
}

// ReconciliationCompositeQuery 描述一次组合匹配的候选范围。
//
// 只做「账号 + 模型 + 输出 token + 时间窗口」这一层粗筛，刻意不在这里约束缓存
// 与输入 token：第 2/3/4 级匹配对缓存的要求各不相同（严格相等 / 只比 cache_read /
// 相差 1），无法用同一条 SQL 谓词表达。把逐级判定留在 Go 侧，保证四级逻辑只有
// 一份实现、可被单元测试覆盖，也避免 SQL 与 Go 两份规则互相漂移。
type ReconciliationCompositeQuery struct {
	AccountIDs   []int64
	Model        string
	OutputTokens int
	OccurredAt   time.Time
	TimeWindow   time.Duration
}

// ReconciliationAccountRuleRepository 账号规则的读写。
type ReconciliationAccountRuleRepository interface {
	List(ctx context.Context) ([]ReconciliationAccountRule, error)
	GetByAccountID(ctx context.Context, accountID int64) (*ReconciliationAccountRule, error)
	Upsert(ctx context.Context, accountID int64, provider, externalKey string, multiplier *float64, enabled bool) (*ReconciliationAccountRule, error)
	Delete(ctx context.Context, accountID int64) (int64, error)
}

// ReconciliationSyncStateRepository 同步状态的读写。
type ReconciliationSyncStateRepository interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	GetMultiple(ctx context.Context, keys []string) (map[string]string, error)
}

// ReconciliationUsageFact 是采集下游用量时需要的原始事实。
//
// 只取三样东西：主键、账号、以及用户实付金额。金额原值留在 usage_logs，
// 采集时只是把它换算成 CNY 并连同汇率冻结进快照表。
type ReconciliationUsageFact struct {
	UsageLogID int64
	AccountID  int64
	ActualCost float64
	CreatedAt  time.Time
}

// ReconciliationUsageSource 提供待采集的下游调用。
type ReconciliationUsageSource interface {
	// ListUsageBetween 返回 [from, to) 内的调用，按发生时间与主键升序。
	ListUsageBetween(ctx context.Context, from, to time.Time, limit int) ([]ReconciliationUsageFact, error)
}

// ReconciliationBillQuery 描述一次上游账单拉取。
//
// TokenName 为空表示拉取该时间窗口内的全部令牌；分页与重试由实现方负责。
type ReconciliationBillQuery struct {
	From      time.Time
	To        time.Time
	TokenName string
	PageSize  int
}

// ReconciliationUpstreamBillSource 拉取上游逐笔账单。
//
// 抽象成端口而不是直接依赖具体的 A6 客户端，好处有两个：
// 匹配与采集逻辑可以脱离网络做单元测试；将来若接入第二个上游，
// 只需再提供一个实现，采集与对账的核心逻辑不受影响。
type ReconciliationUpstreamBillSource interface {
	// FetchBills 拉取并规范化 [From, To) 内的账单。
	FetchBills(ctx context.Context, query ReconciliationBillQuery) ([]ReconciliationUpstreamBillPayload, error)
}

// ReconciliationLedgerRepository 看板的读模型。
//
// 三个方法必须基于同一份分类结果，保证统计与明细自洽。
type ReconciliationLedgerRepository interface {
	// Summary 汇总窗口内指标。
	Summary(ctx context.Context, from, to time.Time) (*ReconciliationSummary, error)
	// Points 按分桶粒度返回趋势点。
	Points(ctx context.Context, from, to time.Time, bucket time.Duration) ([]ReconciliationBucketPoint, error)
	// Rows 返回明细页；status 取 all / matched / unmatched / upstream_unmatched。
	Rows(ctx context.Context, from, to time.Time, status string, page, pageSize int) ([]ReconciliationLedgerRow, int64, error)
	// UsageCountsByAccount 返回窗口内各账号的调用数、首末调用时间与模型名。
	UsageCountsByAccount(ctx context.Context, from, to time.Time) (map[int64]ReconciliationAccountUsage, error)
}

// ReconciliationAccountUsage 是某个账号在窗口内的用量摘要，供规则页展示。
type ReconciliationAccountUsage struct {
	AccountID int64
	Count     int64
	FirstSeen time.Time
	LastSeen  time.Time
	Models    []string
}
