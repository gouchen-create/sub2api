//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 规则页的分组视角 ====================
//
// 背景（生产实测）：一个分组下可能挂多个账号，而每个账号对应不同的上游令牌名
// （分组 #10 下的账号 #47 用 openai-0.1折、账号 #44 用 openai-1折）。
// 管理员是按「分组」这个业务单位理解对账的，因此 List 除了逐账号的 Items，
// 还要给出按分组聚合的 Groups——两者必须是同一次产出的两份视角。
//
// 这组测试锁死：Items 不变（向后兼容）、分组归属沿用 reconcileAccountGroup、
// 无分组账号进 GroupID=0 的特殊分组、两级排序稳定、TokenKeys 去重排序、
// Configured 要求组内全部账号已配置。

// ruleGroupAccountRepo 提供固定账号集。
type ruleGroupAccountRepo struct {
	AccountRepository

	accounts []Account
}

func (r *ruleGroupAccountRepo) ListAllWithFilters(_ context.Context, _, _, _, _ string, _ int64, _ string) ([]Account, error) {
	return r.accounts, nil
}

// ruleGroupRuleRepo 提供固定规则集与分组渠道数。
type ruleGroupRuleRepo struct {
	ReconciliationAccountRuleRepository

	rules              []ReconciliationAccountRule
	groupChannelCounts map[int64]int64
}

func (r *ruleGroupRuleRepo) List(_ context.Context) ([]ReconciliationAccountRule, error) {
	return r.rules, nil
}

func (r *ruleGroupRuleRepo) CountAccountsByGroup(_ context.Context) (map[int64]int64, error) {
	return r.groupChannelCounts, nil
}

// ruleGroupLedgerRepo 只提供规则页用到的两份用量数据。
type ruleGroupLedgerRepo struct {
	ReconciliationLedgerRepository

	usage  map[int64]ReconciliationAccountUsage
	recent map[int64]ReconciliationRecentUsage
}

func (r *ruleGroupLedgerRepo) UsageCountsByAccount(_ context.Context, _, _ time.Time) (map[int64]ReconciliationAccountUsage, error) {
	return r.usage, nil
}

func (r *ruleGroupLedgerRepo) RecentUsageByAccount(_ context.Context) (map[int64]ReconciliationRecentUsage, error) {
	return r.recent, nil
}

// ruleGroupAccount 造一个归属指定分组（priority 固定为 1）的账号。
func ruleGroupAccount(id int64, name string, groupID int64, groupName string) Account {
	account := Account{ID: id, Name: name, Platform: "anthropic", Status: "active", Schedulable: true}
	if groupID > 0 {
		account.AccountGroups = []AccountGroup{{
			AccountID: id, GroupID: groupID, Priority: 1,
			Group: &Group{ID: groupID, Name: groupName},
		}}
	}
	return account
}

func ruleGroupRule(accountID int64, externalKey string, enabled bool) ReconciliationAccountRule {
	return ReconciliationAccountRule{
		ID: accountID, AccountID: accountID, Provider: ReconciliationProviderA6,
		ExternalKey: externalKey, Enabled: enabled, Version: 1,
	}
}

// ruleGroupHarness 组装规则页：两个分组 + 两个不属于任何分组的账号。
//
// 用量刻意造得让两级排序都有区分度：
//   - 分组 2（账号 1、2）合计 7 次，组内 1 > 2
//   - 无分组（账号 4、5）合计 4 次，账号 4 有调用但没配规则
//   - 分组 3（账号 3）合计 3 次
func ruleGroupHarness() (*ReconciliationAccountRuleService, time.Time, time.Time) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	accounts := []Account{
		ruleGroupAccount(1, "主账号", 2, "默认分组"),
		ruleGroupAccount(2, "备用账号", 2, "默认分组"),
		ruleGroupAccount(3, "第三账号", 3, "低价分组"),
		ruleGroupAccount(4, "野生账号", 0, ""),
		ruleGroupAccount(5, "野生账号2", 0, ""),
	}
	rules := []ReconciliationAccountRule{
		// 账号 1 与 2 同属分组 2，但配的是不同的上游令牌名（生产真实形态）；
		// 两个人都写了稳定 ID，用来验证 TokenKeys 的展开与跨账号去重。
		ruleGroupRule(1, "openai-0.1折, id:41210", true),
		ruleGroupRule(2, "id:41210, openai-1折", true),
		ruleGroupRule(3, "gemini", true),
		ruleGroupRule(4, "", true), // 账号 4 有规则行但没填令牌标识 -> 未配置
		// 账号 5 没有任何规则行、也没有调用。
	}
	usage := map[int64]ReconciliationAccountUsage{
		1: {AccountID: 1, Count: 5},
		2: {AccountID: 2, Count: 2},
		3: {AccountID: 3, Count: 3},
		4: {AccountID: 4, Count: 4},
	}

	ledgerSvc := NewReconciliationLedgerService(&ruleGroupLedgerRepo{usage: usage})
	svc := NewReconciliationAccountRuleService(
		&ruleGroupRuleRepo{rules: rules, groupChannelCounts: map[int64]int64{2: 3, 3: 1}},
		&ruleGroupAccountRepo{accounts: accounts},
		ledgerSvc,
	)
	return svc, now.Add(-24 * time.Hour), now
}

// ruleGroupByID 按 GroupID 索引分组，避免断言依赖数组下标（更抗排序调整）。
func ruleGroupByID(t *testing.T, list *ReconciliationAccountRuleList) map[int64]ReconciliationAccountRuleGroupView {
	t.Helper()
	byID := make(map[int64]ReconciliationAccountRuleGroupView, len(list.Groups))
	for _, group := range list.Groups {
		_, duplicated := byID[group.GroupID]
		require.Falsef(t, duplicated, "分组 %d 出现了两次：一个账号只能归一个分组", group.GroupID)
		byID[group.GroupID] = group
	}
	return byID
}

func ruleGroupAccountIDs(group ReconciliationAccountRuleGroupView) []int64 {
	ids := make([]int64, 0, len(group.Accounts))
	for _, account := range group.Accounts {
		ids = append(ids, account.AccountID)
	}
	return ids
}

func TestAccountRuleList_KeepsItemsAndAddsGroups(t *testing.T) {
	svc, from, to := ruleGroupHarness()

	list, err := svc.List(context.Background(), from, to)
	require.NoError(t, err)

	// Items 保持不变：仍然一个账号一行、有用量优先、同档用量降序、再按 ID 升序。
	require.Len(t, list.Items, 5, "Items 必须仍是全部账号（前端旧代码依赖它）")
	itemIDs := make([]int64, 0, len(list.Items))
	for _, item := range list.Items {
		itemIDs = append(itemIDs, item.AccountID)
	}
	assert.Equal(t, []int64{1, 4, 3, 2, 5}, itemIDs)

	// 账号数语义不变：只统计「窗口内有调用但没配好规则」的账号。
	// 账号 4（有规则行但令牌标识为空）算未配置；账号 5 没有调用，不计入。
	assert.EqualValues(t, 1, list.UnconfiguredAccounts)

	require.Len(t, list.Groups, 3, "分组视角：两个真实分组 + 一个「不属于任何分组」")

	// 分组之间：有用量优先 -> 组内用量合计降序 -> GroupID 升序。
	groupIDs := make([]int64, 0, len(list.Groups))
	for _, group := range list.Groups {
		groupIDs = append(groupIDs, group.GroupID)
	}
	assert.Equal(t, []int64{2, 0, 3}, groupIDs)

	groups := ruleGroupByID(t, list)
	assert.EqualValues(t, 7, groups[2].UsageCount)
	assert.EqualValues(t, 4, groups[0].UsageCount)
	assert.EqualValues(t, 3, groups[3].UsageCount)

	// 不属于任何分组的账号必须能被前端显示出来：GroupID=0、名字为空串。
	assert.Equal(t, "", groups[0].GroupName)
	assert.Equal(t, []int64{4, 5}, ruleGroupAccountIDs(groups[0]))
	assert.EqualValues(t, 0, groups[0].GroupChannelCount, "特殊分组在分组表里不存在，渠道数为 0")
}

func TestAccountRuleGroups_InGroupOrderMatchesItems(t *testing.T) {
	svc, from, to := ruleGroupHarness()

	list, err := svc.List(context.Background(), from, to)
	require.NoError(t, err)

	groups := ruleGroupByID(t, list)

	// 组内同样「有用量优先 -> 用量降序 -> 账号 ID 升序」。
	assert.Equal(t, []int64{1, 2}, ruleGroupAccountIDs(groups[2]))
	assert.Equal(t, []int64{4, 5}, ruleGroupAccountIDs(groups[0]))

	// 分组里的账号行必须与 Items 中同一账号的行完全一致（同一次产出，不许两套口径）。
	itemsByAccount := make(map[int64]ReconciliationAccountRuleView, len(list.Items))
	for _, item := range list.Items {
		itemsByAccount[item.AccountID] = item
	}
	for _, group := range list.Groups {
		for _, account := range group.Accounts {
			assert.Equal(t, itemsByAccount[account.AccountID], account,
				"分组内的账号行与 Items 里的同一行必须逐字段一致")
		}
	}

	// 分组视角的账号总数必须等于 Items：不能既算进 A 组又算进 B 组，也不能漏账号。
	grouped := 0
	for _, group := range list.Groups {
		grouped += len(group.Accounts)
	}
	assert.Equal(t, len(list.Items), grouped)

	// 分组的真实渠道数沿用主站口径，而不是本表里的行数。
	assert.EqualValues(t, 3, groups[2].GroupChannelCount)
	assert.EqualValues(t, 1, groups[3].GroupChannelCount)
}

func TestAccountRuleGroups_TokenKeysAreExpandedDedupedAndSorted(t *testing.T) {
	svc, from, to := ruleGroupHarness()

	list, err := svc.List(context.Background(), from, to)
	require.NoError(t, err)

	groups := ruleGroupByID(t, list)

	// 分组 2 的两个账号分别写了 "openai-0.1折, id:41210" 与 "id:41210, openai-1折"：
	// 展开成单个标识、跨账号去重（id:41210 只出现一次）、按字典序排序，
	// 与账号的书写顺序无关。
	assert.Equal(t, []string{"id:41210", "openai-0.1折", "openai-1折"}, groups[2].TokenKeys)
	assert.Equal(t, []string{"gemini"}, groups[3].TokenKeys)
	assert.Empty(t, groups[0].TokenKeys, "未配置的账号不贡献令牌标识")
}

func TestAccountRuleGroups_ConfiguredRequiresEveryAccount(t *testing.T) {
	svc, from, to := ruleGroupHarness()

	list, err := svc.List(context.Background(), from, to)
	require.NoError(t, err)

	groups := ruleGroupByID(t, list)

	// 分组 2、3 内所有账号都已配置；「不属于任何分组」里账号 4 没配好，整组算未配齐。
	assert.True(t, groups[2].Configured)
	assert.True(t, groups[3].Configured)
	assert.False(t, groups[0].Configured)

	// 未配置分组数按分组计，而不是按账号计。
	assert.EqualValues(t, 1, list.UnconfiguredGroups)
}

// 一个账号只出现在它优先级最高的那个分组里（沿用 reconcileAccountGroup），
// 因此分组视角的账号总数必须与 Items 相等——不能既算进 A 组又算进 B 组。
func TestAccountRuleGroups_AccountBelongsToSingleGroup(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	multi := Account{ID: 1, Name: "多分组账号", Platform: "anthropic", Status: "active"}
	multi.AccountGroups = []AccountGroup{
		{AccountID: 1, GroupID: 9, Priority: 5, Group: &Group{ID: 9, Name: "低优先级分组"}},
		{AccountID: 1, GroupID: 2, Priority: 1, Group: &Group{ID: 2, Name: "高优先级分组"}},
	}

	ledgerSvc := NewReconciliationLedgerService(&ruleGroupLedgerRepo{
		usage: map[int64]ReconciliationAccountUsage{1: {AccountID: 1, Count: 4}},
	})
	svc := NewReconciliationAccountRuleService(
		&ruleGroupRuleRepo{rules: []ReconciliationAccountRule{ruleGroupRule(1, "glm", true)}},
		&ruleGroupAccountRepo{accounts: []Account{multi}},
		ledgerSvc,
	)

	list, err := svc.List(context.Background(), now.Add(-time.Hour), now)
	require.NoError(t, err)

	require.Len(t, list.Groups, 1, "账号只归它优先级最高的那个分组")
	assert.EqualValues(t, 2, list.Groups[0].GroupID)
	assert.Equal(t, "高优先级分组", list.Groups[0].GroupName)
	assert.Equal(t, 1, list.Groups[0].GroupPriority)
	assert.EqualValues(t, 4, list.Groups[0].UsageCount, "分组用量是该组账号的合计")
	assert.EqualValues(t, 0, list.UnconfiguredGroups)
}

// 只有「零调用账号没配规则」的分组不算「有调用待配置」。
//
// 口径必须与 UnconfiguredAccounts 一致，也与前端文案「有调用待配置 {count} 个分组」一致：
// 把从没跑过流量的分组算进待办，管理员会去追一个根本不存在的欠账。
func TestAccountRuleGroups_UnconfiguredGroupsOnlyCountsGroupsWithUsage(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	accounts := []Account{
		ruleGroupAccount(1, "已配置账号", 2, "分组二"),
		ruleGroupAccount(2, "零调用未配置账号", 2, "分组二"),
		ruleGroupAccount(3, "有调用未配置账号", 3, "分组三"),
	}
	rules := []ReconciliationAccountRule{ruleGroupRule(1, "glm", true)}
	usage := map[int64]ReconciliationAccountUsage{
		1: {AccountID: 1, Count: 3},
		3: {AccountID: 3, Count: 2},
	}

	ledgerSvc := NewReconciliationLedgerService(&ruleGroupLedgerRepo{usage: usage})
	svc := NewReconciliationAccountRuleService(
		&ruleGroupRuleRepo{rules: rules},
		&ruleGroupAccountRepo{accounts: accounts},
		ledgerSvc,
	)

	list, err := svc.List(context.Background(), now.Add(-time.Hour), now)
	require.NoError(t, err)

	groups := ruleGroupByID(t, list)

	// 两个分组都没「配齐」（组内都有一个账号没有规则）……
	assert.False(t, groups[2].Configured)
	assert.False(t, groups[3].Configured)

	// ……但只有那个「有调用」的分组进入待办计数。
	assert.EqualValues(t, 1, list.UnconfiguredGroups)
	assert.EqualValues(t, 1, list.UnconfiguredAccounts)
}

// 零账号时 Groups 必须是空数组而不是 null（前端不必额外判空）。
func TestAccountRuleGroups_EmptyIsNonNilSlice(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	ledgerSvc := NewReconciliationLedgerService(&ruleGroupLedgerRepo{})
	svc := NewReconciliationAccountRuleService(
		&ruleGroupRuleRepo{}, &ruleGroupAccountRepo{}, ledgerSvc,
	)

	list, err := svc.List(context.Background(), now.Add(-time.Hour), now)
	require.NoError(t, err)
	assert.NotNil(t, list.Groups, "零账号时也要是空数组")
	assert.Empty(t, list.Groups)
	assert.EqualValues(t, 0, list.UnconfiguredGroups)
}
