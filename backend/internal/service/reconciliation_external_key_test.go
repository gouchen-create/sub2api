//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 令牌标识解析 ====================
//
// 背景（生产实测）：上游会改令牌名（历史账单冻结的是 glm-3.5-95%，A6 后台现在叫 glm），
// 而 renamed 之后按名字永远失配。token_id 改名不变，所以规则的 external_key 从
// 「一个令牌名」放宽成「一个或多个令牌标识」。这组测试锁死解析语义：
// 校验、展示、匹配三处共用同一个实现，任何一处自行解释都会造成口径漂移。

func TestParseReconciliationExternalKeys(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantNames []string
		wantIDs   []int64
	}{
		{"空串", "", nil, nil},
		{"只有空白", " \n\t ", nil, nil},
		{"纯令牌名（旧写法，必须继续能解析）", "openai-0.5折", []string{"openai-0.5折"}, nil},
		{"名字 + 稳定 ID", "glm, id:41210", []string{"glm"}, []int64{41210}},
		{"多行粘贴：换行与空格都 trim", "\n  glm  ,\n  id:7  \n", []string{"glm"}, []int64{7}},
		{"id 前缀大小写不敏感", "ID:80246", nil, []int64{80246}},
		{"前缀与数字之间的空白也算", "id:  80246", nil, []int64{80246}},
		{"空项直接丢弃", "a,,b, ,c,", []string{"a", "b", "c"}, nil},
		{"名字去重", "a,a,b", []string{"a", "b"}, nil},
		{"ID 去重（大小写混写也算同一个）", "id:5,id:5,ID:5", nil, []int64{5}},
		{"非法 id 值当普通名字，不丢弃也不报错", "id:abc,id:,id:-1,id:12x,id:0",
			[]string{"id:abc", "id:", "id:-1", "id:12x", "id:0"}, nil},
		{"名字里的空格不被切开", "glm 4.6,id:9", []string{"glm 4.6"}, []int64{9}},
		{"超出 int64 的 ID 当名字", "id:9223372036854775808",
			[]string{"id:9223372036854775808"}, nil},
		{"int64 上限可用", "id:9223372036854775807", nil, []int64{9223372036854775807}},
		{"保持首次出现顺序", "b,id:2,a,id:1", []string{"b", "a"}, []int64{2, 1}},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			names, tokenIDs := ParseReconciliationExternalKeys(testCase.raw)
			assert.Equal(t, testCase.wantNames, names, "令牌名集合不符合预期")
			assert.Equal(t, testCase.wantIDs, tokenIDs, "令牌 ID 集合不符合预期")
		})
	}
}

// ValidateRuleInput 必须接受逗号分隔的多个标识与 id: 形式，
// 但仍然拒绝「什么都解析不出来」的输入——那种规则在页面上显示成已配置，
// 匹配却永远挂不上，比直接报错误导得多。
func TestValidateRuleInput_AllowsMultipleTokenKeys(t *testing.T) {
	key, err := ValidateRuleInput("a6", "  glm , id:41210  ", nil)
	require.NoError(t, err)
	assert.Equal(t, "glm , id:41210", key, "只 trim 整串两端，不改写管理员的原始写法")

	key, err = ValidateRuleInput("a6", ",,id:3,,", nil)
	require.NoError(t, err)
	assert.Equal(t, ",,id:3,,", key, "空项被容忍（解析时丢弃）")

	key, err = ValidateRuleInput("a6", "ID:7", nil)
	require.NoError(t, err)
	assert.Equal(t, "ID:7", key)

	key, err = ValidateRuleInput("", "glm-3.5-95%,id:80246", nil)
	require.NoError(t, err, "provider 缺省视为 a6")
	assert.Equal(t, "glm-3.5-95%,id:80246", key)
}

func TestValidateRuleInput_RejectsKeyWithoutAnyIdentifier(t *testing.T) {
	for _, raw := range []string{",", " , , ", ",,,", "  ,  \n ,  "} {
		_, err := ValidateRuleInput("a6", raw, nil)
		assert.ErrorIsf(t, err, ErrReconciliationTokenNameRequired,
			"输入 %q 解析不出任何标识，必须与空串一样被拒（不能新增错误码）", raw)
	}
}

func TestValidateRuleInput_LengthLimitStillApplies(t *testing.T) {
	// 单个标识超长
	_, err := ValidateRuleInput("a6", strings.Repeat("a", reconciliationMaxExternalKeyLen+1), nil)
	assert.ErrorIs(t, err, ErrReconciliationTokenNameTooLong)

	// 每个标识都很短，但整串超长：列宽限制的是整串，不能只看单个标识
	_, err = ValidateRuleInput("a6", strings.Repeat("ab,", reconciliationMaxExternalKeyLen), nil)
	assert.ErrorIs(t, err, ErrReconciliationTokenNameTooLong)
}

func TestReconciliationRuleMatchesToken(t *testing.T) {
	cases := []struct {
		name        string
		externalKey string
		tokenName   string
		tokenID     int64
		want        bool
	}{
		{"按名字命中", "glm-3.5-95%", "glm-3.5-95%", 0, true},
		{"按稳定 ID 命中（名字已改名）", "glm, id:41210", "glm-3.5-95%", 41210, true},
		{"名字与 ID 都不命中", "glm, id:41210", "glm-3.5-95%", 999, false},
		{"ID 对但规则没写 ID", "glm", "glm-3.5-95%", 41210, false},
		{"名字对但规则没写这个名字", "id:41210", "glm-3.5-95%", 41210, true},
		{"账单没有 ID 时不能凭空命中", "id:41210", "glm", 0, false},
		{"账单没有名字时仍能靠 ID 命中", "id:41210", "", 41210, true},
		{"名字大小写敏感（令牌名是精确标识）", "GLM", "glm", 0, false},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want,
				reconciliationRuleMatchesToken(testCase.externalKey, testCase.tokenName, testCase.tokenID))
		})
	}
}

// 重试守卫（任务 C）与匹配阶段共用同一套判定：停用的规则不算数。
func TestReconciliationRulesMatchToken_IgnoresDisabledRules(t *testing.T) {
	rules := []ReconciliationAccountRule{
		{AccountID: 1, ExternalKey: "glm", Enabled: false},
		{AccountID: 2, ExternalKey: "id:41210", Enabled: true},
	}
	assert.True(t, reconciliationRulesMatchToken(rules, "glm-3.5-95%", 41210), "启用的 ID 规则命中")

	onlyDisabled := []ReconciliationAccountRule{{AccountID: 1, ExternalKey: "glm", Enabled: false}}
	assert.False(t, reconciliationRulesMatchToken(onlyDisabled, "glm", 0), "停用的规则不得让账单复活")

	assert.False(t, reconciliationRulesMatchToken(nil, "", 0), "既无名字也无 ID 的账单不可重试")
}

// ==================== raw 报文里的稳定标识 ====================

func TestReconciliationTokenIDFromRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want int64
	}{
		{"json.Number（A6 客户端 UseNumber 解出的形态）", map[string]any{"token_id": json.Number("41210")}, 41210},
		{"float64（手工重放/测试直接构造 map 的形态）", map[string]any{"token_id": float64(41210)}, 41210},
		{"字符串数字", map[string]any{"token_id": "41210"}, 41210},
		{"驼峰写法", map[string]any{"tokenId": json.Number("7")}, 7},
		{"字段缺失", map[string]any{"token_name": "glm"}, 0},
		{"显式 null", map[string]any{"token_id": nil}, 0},
		{"非数字", map[string]any{"token_id": "abc"}, 0},
		{"零与负数都不算有效标识", map[string]any{"token_id": json.Number("0")}, 0},
		{"负数", map[string]any{"token_id": float64(-3)}, 0},
		{"空报文", nil, 0},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, reconciliationTokenIDFromRaw(testCase.raw))
		})
	}
}

func TestReconciliationPayloadTokenID_PrefersParsedField(t *testing.T) {
	// 已经解析好的值优先：raw 里就算有别的 ID 也不能覆盖调用方给的结果。
	payload := &ReconciliationUpstreamBillPayload{
		TokenID: 999,
		Raw:     map[string]any{"token_id": json.Number("1")},
	}
	assert.Equal(t, int64(999), reconciliationPayloadTokenID(payload))

	// 没有预先解析时从 raw 兜底（匹配阶段读数据库回来的账单就是这条路径）。
	fallback := &ReconciliationUpstreamBillPayload{Raw: map[string]any{"token_id": json.Number("41210")}}
	assert.Equal(t, int64(41210), reconciliationPayloadTokenID(fallback))
	assert.Equal(t, int64(0), reconciliationPayloadTokenID(nil))
}

// ==================== 导入路径与匹配路径 ====================

// externalKeyImportRepo 记录服务真正交给仓库的账单，用来断言导入前已解析出 token_id。
type externalKeyImportRepo struct {
	ReconciliationUpstreamBillRepository

	bills []ReconciliationUpstreamBillPayload
}

func (r *externalKeyImportRepo) UpsertBatch(_ context.Context, bills []ReconciliationUpstreamBillPayload) (int64, error) {
	r.bills = append(r.bills, bills...)
	return int64(len(bills)), nil
}

func TestImportBills_ParsesStableTokenIDFromRaw(t *testing.T) {
	repo := &externalKeyImportRepo{}
	svc := NewReconciliationSyncService(
		&collectCursorExtrasRepo{}, repo, &collectCursorRuleRepo{}, newCollectCursorStateRepo(),
		nil, nil, ReconciliationSyncConfig{FxUSDCNYRate: 1},
	)

	bills := []ReconciliationUpstreamBillPayload{
		{
			Provider: ReconciliationProviderA6, UpstreamRequestID: "req-number",
			TokenName: "glm-3.5-95%", CostOriginal: 1,
			Raw: map[string]any{"token_id": json.Number("41210"), "token_name": "glm-3.5-95%"},
		},
		{
			Provider: ReconciliationProviderA6, UpstreamRequestID: "req-float",
			TokenName: "deepseek-3.5-95%", CostOriginal: 1,
			Raw: map[string]any{"token_id": float64(80246)},
		},
		{
			Provider: ReconciliationProviderA6, UpstreamRequestID: "req-no-raw",
			TokenName: "manual-import", CostOriginal: 1,
		},
		{
			Provider: ReconciliationProviderA6, UpstreamRequestID: "req-preset",
			TokenName: "preset", CostOriginal: 1, TokenID: 999,
			Raw: map[string]any{"token_id": json.Number("1")},
		},
	}

	inserted, err := svc.importBills(context.Background(), bills)
	require.NoError(t, err)
	require.EqualValues(t, 4, inserted)
	require.Len(t, repo.bills, 4)

	assert.Equal(t, int64(41210), repo.bills[0].TokenID, "json.Number 形态必须解析出来")
	assert.Equal(t, int64(80246), repo.bills[1].TokenID, "float64 形态必须解析出来")
	assert.Equal(t, int64(0), repo.bills[2].TokenID, "没有 raw 的账单退回按名字匹配")
	assert.Equal(t, int64(999), repo.bills[3].TokenID, "调用方已解析好的值不被 raw 覆盖")
}

// externalKeyRuleRepo 提供固定规则集。
type externalKeyRuleRepo struct {
	ReconciliationAccountRuleRepository

	rules []ReconciliationAccountRule
}

func (r *externalKeyRuleRepo) List(_ context.Context) ([]ReconciliationAccountRule, error) {
	return r.rules, nil
}

// externalKeyMatchBillRepo 记录匹配结果，并只回报预置的候选调用。
type externalKeyMatchBillRepo struct {
	ReconciliationUpstreamBillRepository

	staging    []ReconciliationUpstreamBill
	candidates []ReconciliationMatchCandidate
	matched    []int64
	unmatched  []int64
}

func (r *externalKeyMatchBillRepo) ListStaging(_ context.Context, _, _ time.Time, _ int) ([]ReconciliationUpstreamBill, error) {
	return r.staging, nil
}

func (r *externalKeyMatchBillRepo) FindDirectMatchCandidates(_ context.Context, _ string) ([]ReconciliationMatchCandidate, error) {
	// 第 1 级返回空：本组测试要验证的是组合匹配前的账号解析（名字 + 稳定 ID）。
	return nil, nil
}

func (r *externalKeyMatchBillRepo) FindCompositeMatchCandidates(_ context.Context, _ ReconciliationCompositeQuery) ([]ReconciliationMatchCandidate, error) {
	return r.candidates, nil
}

func (r *externalKeyMatchBillRepo) MarkMatched(_ context.Context, billID, _, _ int64, _ string) error {
	r.matched = append(r.matched, billID)
	return nil
}

func (r *externalKeyMatchBillRepo) MarkUnmatched(_ context.Context, billIDs []int64) error {
	r.unmatched = append(r.unmatched, billIDs...)
	return nil
}

// renamedTokenBill 造一条「账单里冻结的是老令牌名、稳定标识只在 raw 里」的账单。
func renamedTokenBill(importedAt time.Time, occurredAt time.Time) ReconciliationUpstreamBill {
	return ReconciliationUpstreamBill{
		ID:         1,
		ImportedAt: importedAt,
		Payload: ReconciliationUpstreamBillPayload{
			Provider: ReconciliationProviderA6,
			// 故意不给上游请求 ID：第 1 级直接匹配不参与，走账号解析 + 组合匹配。
			UpstreamRequestID:   "",
			OccurredAt:          occurredAt,
			Model:               "glm-4.6",
			TokenName:           "glm-3.5-95%",
			InputTokens:         1000,
			OutputTokens:        200,
			CacheReadTokens:     10,
			CacheCreationTokens: 5,
			CacheTokensTotal:    15,
			// raw 由 ListStaging 从数据库带回来，稳定标识只存在于这里。
			Raw: map[string]any{"token_id": json.Number("41210"), "token_name": "glm-3.5-95%"},
		},
	}
}

func renamedTokenCandidate(occurredAt time.Time) ReconciliationMatchCandidate {
	return ReconciliationMatchCandidate{
		UsageLogID: 555, AccountID: 47,
		InputTokens: 1000, CacheReadTokens: 10, CacheCreationTok: 5,
		CreatedAt: occurredAt,
	}
}

func newRenamedTokenHarness(bill ReconciliationUpstreamBill, rules []ReconciliationAccountRule) (*ReconciliationSyncService, *externalKeyMatchBillRepo) {
	billRepo := &externalKeyMatchBillRepo{
		staging:    []ReconciliationUpstreamBill{bill},
		candidates: []ReconciliationMatchCandidate{renamedTokenCandidate(bill.Payload.OccurredAt)},
	}
	svc := NewReconciliationSyncService(
		&graceTestExtrasRepo{},
		billRepo,
		&externalKeyRuleRepo{rules: rules},
		newCollectCursorStateRepo(),
		nil, nil,
		ReconciliationSyncConfig{
			FxUSDCNYRate:     1,
			StagingLimit:     100,
			MatchGracePeriod: 30 * time.Minute,
		},
	)
	return svc, billRepo
}

// TestMatchStaging_MatchesRenamedTokenByStableTokenID 是任务 A 的核心回归测试。
//
// 账单里冻结的令牌名是 glm-3.5-95%，A6 后台现在叫 glm——按名字怎么都对不上。
// 规则写成 "glm, id:41210" 之后，匹配必须靠 raw 报文里的 token_id 找回这个账号。
//
// 修复前这个测试必然失败：旧实现只做 rule.ExternalKey == tokenName 的精确比较，
// 既不认逗号分隔，也不看 token_id，账单会一直退回孤儿。
func TestMatchStaging_MatchesRenamedTokenByStableTokenID(t *testing.T) {
	now := time.Now().UTC()
	occurredAt := now.Add(-time.Hour)

	svc, billRepo := newRenamedTokenHarness(
		renamedTokenBill(now.Add(-time.Minute), occurredAt),
		[]ReconciliationAccountRule{{
			ID: 9, AccountID: 47, Provider: ReconciliationProviderA6,
			ExternalKey: "glm, id:41210", Enabled: true,
		}},
	)

	matched, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, matched, "改名后的账单必须能靠 token_id 找到账号并匹配上")
	assert.EqualValues(t, 0, orphaned)
	assert.Equal(t, []int64{1}, billRepo.matched)
	assert.Empty(t, billRepo.unmatched)
}

// 反面对照：规则里只写当前名字（没有稳定标识）时，这条历史账单依然匹配不上，
// 说明上一条测试通过的原因确实是 token_id，而不是别的巧合。
func TestMatchStaging_RenamedTokenWithoutStableIDStaysOrphan(t *testing.T) {
	now := time.Now().UTC()
	occurredAt := now.Add(-time.Hour)

	// 导入时间早于宽限期：匹配不上就要被判成孤儿（真实的老账单形态）。
	svc, billRepo := newRenamedTokenHarness(
		renamedTokenBill(now.Add(-2*time.Hour), occurredAt),
		[]ReconciliationAccountRule{{
			ID: 9, AccountID: 47, Provider: ReconciliationProviderA6,
			ExternalKey: "glm", Enabled: true,
		}},
	)

	matched, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, matched)
	assert.EqualValues(t, 1, orphaned)
	assert.Equal(t, []int64{1}, billRepo.unmatched)
}

// 账号解析同时接受名字与 ID：规则里两个都写时，任一命中都算候选，
// 且同名账号只返回一次。
func TestResolveAccountIDsForToken_MergesNameAndIDChannels(t *testing.T) {
	svc := NewReconciliationSyncService(
		&graceTestExtrasRepo{}, &externalKeyMatchBillRepo{},
		&externalKeyRuleRepo{rules: []ReconciliationAccountRule{
			{AccountID: 47, Provider: ReconciliationProviderA6, ExternalKey: "glm, id:41210", Enabled: true},
			{AccountID: 44, Provider: ReconciliationProviderA6, ExternalKey: "id:41210", Enabled: true},
			{AccountID: 45, Provider: ReconciliationProviderA6, ExternalKey: "glm-3.5-95%", Enabled: true},
			{AccountID: 46, Provider: ReconciliationProviderA6, ExternalKey: "glm, id:41210", Enabled: false},
			{AccountID: 48, Provider: ReconciliationProviderA6, ExternalKey: "other", Enabled: true},
		}},
		newCollectCursorStateRepo(), nil, nil, ReconciliationSyncConfig{FxUSDCNYRate: 1},
	)

	ids, err := svc.resolveAccountIDsForToken(context.Background(), "glm-3.5-95%", 41210)
	require.NoError(t, err)
	assert.Equal(t, []int64{47, 44, 45}, ids, "ID 与名字两条通道合并去重，停用的规则不参与")

	// 账单没有稳定标识时（历史手工导入的报文里没有 token_id）退回纯名字匹配。
	ids, err = svc.resolveAccountIDsForToken(context.Background(), "glm", 0)
	require.NoError(t, err)
	assert.Equal(t, []int64{47}, ids)
}
