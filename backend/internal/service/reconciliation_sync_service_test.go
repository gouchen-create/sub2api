//go:build unit

package service

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reconciliationTestPayload() *ReconciliationUpstreamBillPayload {
	return &ReconciliationUpstreamBillPayload{
		Provider:            ReconciliationProviderA6,
		UpstreamRequestID:   "req-1",
		OccurredAt:          time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
		Model:               "claude-sonnet-4.5",
		TokenName:           "0.12",
		InputTokens:         1000,
		OutputTokens:        200,
		CacheReadTokens:     300,
		CacheCreationTokens: 50,
		CacheTokensTotal:    350,
	}
}

// 上游只回一个合并缓存值时，分列为 0、合计保留真值。
// 这是最容易被忽略的一种账单形态：若拿分列的 0 去比对，这类账单会全部匹配失败。
func TestBillCacheTotal_PrefersExplicitTotal(t *testing.T) {
	combined := &ReconciliationUpstreamBillPayload{
		CacheReadTokens:     0,
		CacheCreationTokens: 0,
		CacheTokensTotal:    350,
	}
	assert.Equal(t, 350, billCacheTotal(combined), "只回合并值时必须采用合计")

	split := &ReconciliationUpstreamBillPayload{
		CacheReadTokens:     300,
		CacheCreationTokens: 50,
		CacheTokensTotal:    0,
	}
	assert.Equal(t, 350, billCacheTotal(split), "分列齐全时合计为两者之和")
}

func TestCandidateCacheTotal(t *testing.T) {
	candidate := ReconciliationMatchCandidate{CacheReadTokens: 300, CacheCreationTok: 50}
	assert.Equal(t, 350, candidateCacheTotal(candidate))
}

// 第 2 级匹配：缓存合计与输入 token 都对得上才算命中。
// 上游只回合并值的那条账单也必须能匹配上——这是本次改造的核心修正点。
func TestFilterExactTokenMatches_CombinedCacheStillMatches(t *testing.T) {
	payload := reconciliationTestPayload()
	payload.CacheReadTokens = 0
	payload.CacheCreationTokens = 0
	payload.CacheTokensTotal = 350

	matching := ReconciliationMatchCandidate{InputTokens: 1000, CacheReadTokens: 300, CacheCreationTok: 50}
	otherCache := ReconciliationMatchCandidate{InputTokens: 1000, CacheReadTokens: 300, CacheCreationTok: 51}
	otherInput := ReconciliationMatchCandidate{InputTokens: 999, CacheReadTokens: 300, CacheCreationTok: 50}

	result := filterExactTokenMatches([]ReconciliationMatchCandidate{matching, otherCache, otherInput}, payload)
	require.Len(t, result, 1)
	assert.Equal(t, 1000, result[0].InputTokens)
	assert.Equal(t, 50, result[0].CacheCreationTok)
}

// 输入 token 的两种等价写法都要认：上游可能只算纯输入，也可能把缓存并入输入。
func TestInputTokensEquivalent(t *testing.T) {
	assert.True(t, inputTokensEquivalent(1000, 1000, 300), "完全相同")
	assert.True(t, inputTokensEquivalent(1300, 1000, 300), "把缓存读取并入输入的写法")
	assert.False(t, inputTokensEquivalent(1001, 1000, 300))
	assert.False(t, inputTokensEquivalent(999, 1000, 300))
}

// 上游只回合并值时没有独立的「读」值，第 3 级必须直接放弃，
// 而不是拿 0 去比出一个假的相等。
func TestFilterCacheReadMatches_SkipsWhenOnlyCombined(t *testing.T) {
	payload := reconciliationTestPayload()
	payload.CacheReadTokens = 0
	payload.CacheCreationTokens = 0
	payload.CacheTokensTotal = 350

	candidates := []ReconciliationMatchCandidate{
		{InputTokens: 1000, CacheReadTokens: 0, CacheCreationTok: 350},
	}
	assert.Nil(t, filterCacheReadMatches(candidates, payload),
		"只回合并值时应跳过第 3 级，交给第 4 级容差兜底")
}

func TestFilterCacheReadMatches_MatchesOnReadOnly(t *testing.T) {
	payload := reconciliationTestPayload()
	// 缓存写入与账单不同，但读取一致 -> 第 3 级应当接受。
	candidates := []ReconciliationMatchCandidate{
		{InputTokens: 1000, CacheReadTokens: 300, CacheCreationTok: 999},
		{InputTokens: 1000, CacheReadTokens: 301, CacheCreationTok: 50},
	}
	result := filterCacheReadMatches(candidates, payload)
	require.Len(t, result, 1)
	assert.Equal(t, 999, result[0].CacheCreationTok)
}

func TestPickUniqueClosest(t *testing.T) {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	assert.Nil(t, pickUniqueClosest(nil, base), "没有候选时返回 nil")

	unique := []ReconciliationMatchCandidate{
		{CreatedAt: base.Add(-90 * time.Second)},
		{CreatedAt: base.Add(5 * time.Second)},
	}
	picked := pickUniqueClosest(unique, base)
	require.NotNil(t, picked)
	assert.Equal(t, base.Add(5*time.Second), picked.CreatedAt)

	// 时间差并列意味着无法唯一确定是哪一次调用，必须拒绝而不是随便挑一个。
	tied := []ReconciliationMatchCandidate{
		{CreatedAt: base.Add(-10 * time.Second)},
		{CreatedAt: base.Add(10 * time.Second)},
	}
	assert.Nil(t, pickUniqueClosest(tied, base), "最小时间差并列时必须拒绝")
}

// 第 4 级是窄兜底：缓存恰好差 1、时间差不超过 2 秒、候选严格唯一，缺一不可。
func TestMatchCacheTolerance(t *testing.T) {
	collector := &ReconciliationSyncService{}
	payload := reconciliationTestPayload()
	payload.OccurredAt = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	withinWindow := ReconciliationMatchCandidate{
		CacheReadTokens: 300, CacheCreationTok: 51,
		CreatedAt: payload.OccurredAt.Add(1 * time.Second),
	}
	picked := collector.matchCacheTolerance([]ReconciliationMatchCandidate{withinWindow}, payload)
	require.NotNil(t, picked, "缓存差 1 且在 2 秒内、候选唯一 -> 应当命中")

	tooLate := withinWindow
	tooLate.CreatedAt = payload.OccurredAt.Add(3 * time.Second)
	assert.Nil(t, collector.matchCacheTolerance([]ReconciliationMatchCandidate{tooLate}, payload),
		"超出 2 秒窗口必须放弃")

	twoApart := ReconciliationMatchCandidate{
		CacheReadTokens: 300, CacheCreationTok: 52,
		CreatedAt: payload.OccurredAt,
	}
	assert.Nil(t, collector.matchCacheTolerance([]ReconciliationMatchCandidate{twoApart}, payload),
		"缓存相差 2 不算容差")

	// 两个都符合条件时无法唯一确定，必须放弃。
	second := withinWindow
	second.CreatedAt = payload.OccurredAt.Add(-1 * time.Second)
	assert.Nil(t, collector.matchCacheTolerance([]ReconciliationMatchCandidate{withinWindow, second}, payload),
		"候选不唯一时必须放弃")

	// 账单缓存为 0 时整个容差判定没有意义。
	noCache := reconciliationTestPayload()
	noCache.CacheReadTokens = 0
	noCache.CacheCreationTokens = 0
	noCache.CacheTokensTotal = 0
	assert.Nil(t, collector.matchCacheTolerance([]ReconciliationMatchCandidate{withinWindow}, noCache),
		"账单缓存为 0 时不做容差匹配")
}

func TestSelectBucket(t *testing.T) {
	start := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		span time.Duration
		want string
	}{
		{2 * time.Hour, "1小时"},
		{24 * time.Hour, "1小时"},
		{48 * time.Hour, "1小时"},
		{7 * 24 * time.Hour, "6小时"},
		{30 * 24 * time.Hour, "1天"},
		{200 * 24 * time.Hour, "1周"},
	}
	for _, tc := range cases {
		got := SelectBucket(start, start.Add(tc.span))
		assert.Equal(t, tc.want, got.Label, "窗口 %s 的分桶", tc.span)
	}
}

// 窗口缺省为最近 24 小时；倒置或过长必须报错而不是静默改成别的区间。
func TestResolveWindow(t *testing.T) {
	service := &ReconciliationLedgerService{}

	from, to, err := service.ResolveWindow(nil, nil)
	require.NoError(t, err)
	assert.WithinDuration(t, to.Add(-ReconciliationDefaultWindow), from, time.Second)

	inverted := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _, err = service.ResolveWindow(&inverted, &inverted)
	assert.ErrorIs(t, err, ErrReconciliationInvalidWindow, "起止相同视为非法窗口")

	tooLongFrom := to.Add(-400 * 24 * time.Hour)
	_, _, err = service.ResolveWindow(&tooLongFrom, &to)
	assert.ErrorIs(t, err, ErrReconciliationInvalidWindow, "超过上限的窗口必须报错")
}

// 六种成本来源都必须有非空中文标签：前端在遇到未知 cost_source 时会回落到这个字符串，
// 空标签会让状态列变成一片空白。
func TestReconciliationCostSourceLabel_AllNonEmpty(t *testing.T) {
	sources := []ReconciliationCostSource{
		ReconciliationCostSourceBilled,
		ReconciliationCostSourceA6Waiting,
		ReconciliationCostSourceA6Pending,
		ReconciliationCostSourceRuleUnconfigured,
		ReconciliationCostSourceUpstreamUnmatched,
		ReconciliationCostSourcePending,
		ReconciliationCostSource("从未见过的取值"),
	}
	for _, source := range sources {
		assert.NotEmpty(t, ReconciliationCostSourceLabel(source), "来源 %q 的标签不能为空", source)
	}
}

// 毛利只对已对账的行计算。
//
// 孤儿账单行有成本、没有对应的下游收入，算出来会是一个负的成本数字，
// 误导性很强，因此必须返回「未知」而不是一个负数。
func TestReconciliationGrossProfitCNY(t *testing.T) {
	matched := ReconciliationLedgerRow{Matched: true, RevenueCNY: 10, UpstreamCostCNY: 4}
	profit, ok := matched.ReconciliationGrossProfitCNY()
	require.True(t, ok)
	assert.InDelta(t, 6.0, profit, 1e-9)

	unmatched := ReconciliationLedgerRow{Matched: false, RevenueCNY: 10, UpstreamCostCNY: 4}
	_, ok = unmatched.ReconciliationGrossProfitCNY()
	assert.False(t, ok, "未对账的行没有毛利")

	orphan := ReconciliationLedgerRow{
		RecordType:      ReconciliationRecordTypeUpstreamUnmatched,
		Matched:         false,
		HasUpstreamCost: true,
		UpstreamCostCNY: 7,
	}
	_, ok = orphan.ReconciliationGrossProfitCNY()
	assert.False(t, ok, "孤儿账单行不能报出负毛利")
}

// 规则校验：Subarx 已下线，继续接受它会写出永远不被采集器读取的规则，
// 管理员却以为配置生效了。
func TestValidateRuleInput(t *testing.T) {
	key, err := ValidateRuleInput("a6", " 0.12 ", nil)
	require.NoError(t, err)
	assert.Equal(t, "0.12", key, "令牌名应被去除首尾空白")

	key, err = ValidateRuleInput("", "0.12", nil)
	require.NoError(t, err)
	assert.Equal(t, "0.12", key, "provider 缺省视为 a6")

	_, err = ValidateRuleInput("subarx", "x", nil)
	assert.ErrorIs(t, err, ErrReconciliationProviderUnsupported)

	_, err = ValidateRuleInput("a6", "   ", nil)
	assert.ErrorIs(t, err, ErrReconciliationTokenNameRequired)

	_, err = ValidateRuleInput("a6", strings.Repeat("a", reconciliationMaxExternalKeyLen+1), nil)
	assert.ErrorIs(t, err, ErrReconciliationTokenNameTooLong)
}

// 匹配方式的说明文案要么有值要么为空，不能返回未替换的占位符。
func TestDescribeMatchMethod(t *testing.T) {
	for _, method := range []string{
		ReconciliationMatchDirectRequestID,
		ReconciliationMatchCompositeTokensTime,
		ReconciliationMatchCompositeCacheRead,
		ReconciliationMatchCompositeCacheTolerance,
	} {
		assert.NotEmpty(t, DescribeMatchMethod(method), "匹配方式 %q 应有中文说明", method)
	}
	assert.Empty(t, DescribeMatchMethod("nonsense"))
}
