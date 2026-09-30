package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// 上游提供方取值。Subarx 已下线，只保留 A6。
const (
	ReconciliationProviderA6 = "a6"
)

// 规则校验错误。reason 用 UPPER_SNAKE，符合仓库错误约定；
// 接口层会把这些 reason 与前端需要的 COMPANION_* 提示同时带给前端。
var (
	// ErrReconciliationProviderUnsupported 表示填了一个已下线的上游提供方。
	ErrReconciliationProviderUnsupported = errors.New("RECONCILIATION_PROVIDER_UNSUPPORTED")
	// ErrReconciliationTokenNameRequired 表示 A6 规则缺少令牌名。
	ErrReconciliationTokenNameRequired = errors.New("RECONCILIATION_TOKEN_NAME_REQUIRED")
	// ErrReconciliationTokenNameTooLong 表示令牌名超过上游列宽。
	ErrReconciliationTokenNameTooLong = errors.New("RECONCILIATION_TOKEN_NAME_TOO_LONG")
)

// reconciliationMaxExternalKeyLen 与 reconciliation_account_rules.external_key 列宽对齐。
const reconciliationMaxExternalKeyLen = 128

// ReconciliationAccountRuleView 是规则页的一行。
//
// 它按「账号」而不是「规则」组织：没有规则的账号同样会出现，这样管理员才能在
// 面板上为任何一个账号补规则。列表永远不空，避免出现无从下手的空页面。
type ReconciliationAccountRuleView struct {
	AccountID   int64
	Provider    string
	ExternalKey string
	Multiplier  *float64
	Version     int64
	Enabled     bool
	Configured  bool
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// Current 表示该账号当前是否处于可用调度状态。
	Current bool

	GroupID       int64
	GroupName     string
	GroupPriority int

	AccountName        string
	AccountPlatform    string
	AccountStatus      string
	AccountSchedulable bool

	UsageCount int64
	FirstSeen  *time.Time
	LastSeen   *time.Time
	Models     []string
}

// ReconciliationAccountRuleList 是规则页的完整响应载荷。
type ReconciliationAccountRuleList struct {
	From                 time.Time
	To                   time.Time
	UnconfiguredAccounts int64
	Items                []ReconciliationAccountRuleView
}

// ReconciliationAccountRuleService 负责账号规则的读写与规则页数据组装。
type ReconciliationAccountRuleService struct {
	ruleRepo    ReconciliationAccountRuleRepository
	accountRepo AccountRepository
	ledgerSvc   *ReconciliationLedgerService
}

// NewReconciliationAccountRuleService 创建账号规则服务。
func NewReconciliationAccountRuleService(
	ruleRepo ReconciliationAccountRuleRepository,
	accountRepo AccountRepository,
	ledgerSvc *ReconciliationLedgerService,
) *ReconciliationAccountRuleService {
	return &ReconciliationAccountRuleService{
		ruleRepo:    ruleRepo,
		accountRepo: accountRepo,
		ledgerSvc:   ledgerSvc,
	}
}

// ValidateRuleInput 校验并规范化规则入参。
//
// provider 只接受 a6：Subarx 已取消，继续接受它会写出永远不会被采集器读取的规则，
// 管理员却以为配置生效了。这里直接报错比静默接受诚实。
func ValidateRuleInput(provider, externalKey string, multiplier *float64) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		normalized = ReconciliationProviderA6
	}
	if normalized != ReconciliationProviderA6 {
		return "", ErrReconciliationProviderUnsupported
	}

	key := strings.TrimSpace(externalKey)
	if key == "" {
		return "", ErrReconciliationTokenNameRequired
	}
	if len(key) > reconciliationMaxExternalKeyLen {
		return "", ErrReconciliationTokenNameTooLong
	}
	// multiplier 目前只做长度与符号校验：Subarx 下线后它已不参与对账口径，
	// 保留字段是为了兼容前端契约与将来的倍率需求。
	if multiplier != nil && *multiplier < 0 {
		return "", ErrReconciliationProviderUnsupported
	}
	return key, nil
}

// List 组装规则页数据：全部账号 + 已有规则 + 窗口内用量。
func (s *ReconciliationAccountRuleService) List(ctx context.Context, from, to time.Time) (*ReconciliationAccountRuleList, error) {
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, "", "", "", "", 0, "")
	if err != nil {
		return nil, err
	}

	rules, err := s.ruleRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	rulesByAccount := make(map[int64]ReconciliationAccountRule, len(rules))
	for _, rule := range rules {
		rulesByAccount[rule.AccountID] = rule
	}

	usageByAccount, err := s.ledgerSvc.UsageCountsByAccount(ctx, from, to)
	if err != nil {
		return nil, err
	}

	items := make([]ReconciliationAccountRuleView, 0, len(accounts))
	var unconfigured int64
	for i := range accounts {
		account := &accounts[i]
		view := ReconciliationAccountRuleView{
			AccountID:          account.ID,
			Provider:           "",
			ExternalKey:        "",
			Configured:         false,
			Current:            strings.EqualFold(account.Status, "active"),
			AccountName:        account.Name,
			AccountPlatform:    account.Platform,
			AccountStatus:      account.Status,
			AccountSchedulable: account.Schedulable,
		}
		view.GroupID, view.GroupName, view.GroupPriority = reconcileAccountGroup(account)

		if rule, ok := rulesByAccount[account.ID]; ok {
			view.Provider = rule.Provider
			view.ExternalKey = rule.ExternalKey
			view.Multiplier = rule.Multiplier
			view.Version = rule.Version
			view.Enabled = rule.Enabled
			view.Configured = rule.Enabled && strings.TrimSpace(rule.ExternalKey) != ""
			view.CreatedAt = rule.CreatedAt
			view.UpdatedAt = rule.UpdatedAt
		}

		if usage, ok := usageByAccount[account.ID]; ok {
			view.UsageCount = usage.Count
			view.Models = usage.Models
			firstSeen, lastSeen := usage.FirstSeen, usage.LastSeen
			view.FirstSeen = &firstSeen
			view.LastSeen = &lastSeen
			if !view.Configured {
				unconfigured++
			}
		}

		items = append(items, view)
	}

	// 有调用的账号排在前面，方便管理员优先处理真正在跑流量的账号；
	// 同组内按调用量降序，再按账号 ID 升序保证顺序稳定。
	sort.SliceStable(items, func(i, j int) bool {
		if (items[i].UsageCount > 0) != (items[j].UsageCount > 0) {
			return items[i].UsageCount > 0
		}
		if items[i].UsageCount != items[j].UsageCount {
			return items[i].UsageCount > items[j].UsageCount
		}
		return items[i].AccountID < items[j].AccountID
	})

	return &ReconciliationAccountRuleList{
		From:                 from,
		To:                   to,
		UnconfiguredAccounts: unconfigured,
		Items:                items,
	}, nil
}

// reconcileAccountGroup 从一个账号的多个分组中挑出用于展示的那一个。
//
// 账号可以同时属于多个分组，而规则页每行只能显示一个。取优先级数值最小的
// （本仓库约定数值越小优先级越高），并以分组 ID 兜底排序，保证展示稳定不跳动。
func reconcileAccountGroup(account *Account) (int64, string, int) {
	if account == nil || len(account.AccountGroups) == 0 {
		return 0, "", 0
	}
	groups := make([]AccountGroup, len(account.AccountGroups))
	copy(groups, account.AccountGroups)
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Priority != groups[j].Priority {
			return groups[i].Priority < groups[j].Priority
		}
		return groups[i].GroupID < groups[j].GroupID
	})

	selected := groups[0]
	name := ""
	if selected.Group != nil {
		name = selected.Group.Name
	}
	return selected.GroupID, name, selected.Priority
}

// Upsert 保存账号规则并返回保存后的视图。
func (s *ReconciliationAccountRuleService) Upsert(ctx context.Context, accountID int64, provider, externalKey string, multiplier *float64, enabled bool) (*ReconciliationAccountRuleView, error) {
	key, err := ValidateRuleInput(provider, externalKey, multiplier)
	if err != nil {
		return nil, err
	}

	rule, err := s.ruleRepo.Upsert(ctx, accountID, ReconciliationProviderA6, key, multiplier, enabled)
	if err != nil {
		return nil, err
	}

	return &ReconciliationAccountRuleView{
		AccountID:   rule.AccountID,
		Provider:    rule.Provider,
		ExternalKey: rule.ExternalKey,
		Multiplier:  rule.Multiplier,
		Version:     rule.Version,
		Enabled:     rule.Enabled,
		Configured:  rule.Enabled && strings.TrimSpace(rule.ExternalKey) != "",
		CreatedAt:   rule.CreatedAt,
		UpdatedAt:   rule.UpdatedAt,
	}, nil
}

// Delete 删除账号规则；账号本来没有规则时返回 0，不算错误。
func (s *ReconciliationAccountRuleService) Delete(ctx context.Context, accountID int64) (int64, error) {
	return s.ruleRepo.Delete(ctx, accountID)
}
