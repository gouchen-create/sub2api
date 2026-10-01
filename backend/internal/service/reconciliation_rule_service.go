package service

import (
	"context"
	"errors"
	"sort"
	"strconv"
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

// reconciliationExternalKeySeparator 分隔同一条规则里的多个令牌标识。
//
// 刻意用英文逗号而不是空格：令牌名本身可能含空格（例如 "glm 4.6"），
// 用空格分隔会把一个名字劈成两段永远匹配不上的碎片。
const reconciliationExternalKeySeparator = ","

// reconciliationExternalKeyIDPrefix 是「按上游 token_id 匹配」的标识前缀。
const reconciliationExternalKeyIDPrefix = "id:"

// ParseReconciliationExternalKeys 把规则的 external_key 拆成「令牌名」与「上游令牌 ID」两组标识。
//
// 为什么要有这个函数：上游会改令牌名（历史账单里冻结的是 glm-3.5-95%，A6 后台现在叫 glm），
// 而 token_id 改名不变，是稳定的标识。所以 external_key 的语义从「一个令牌名」
// 放宽成「一个或多个令牌标识」，写法为英文逗号分隔，每个标识允许两种形式：
//
//	openai-0.5折   —— 纯令牌名，按名字匹配（历史账单里冻结的就是当时那个名字）
//	id:80246       —— 按上游 token_id 匹配，改名后依然有效
//
// 解析规则刻意宽容，因为这个字符串由管理员手工填写：
//   - 每项 trim 掉首尾空白（含换行，方便从后台多行粘贴）；
//   - 空项（连续逗号、首尾逗号、纯空格项）直接丢弃，不报错；
//   - "id:" 前缀大小写不敏感（ID:80246 与 id:80246 等价）；
//   - "id:" 后面不是合法正整数的（例如 id:abc、id:、id:-1）当作**普通令牌名**而不是
//     报错丢弃——以 "id:" 开头的令牌名是合法的，静默丢掉会让管理员以为配置生效了。
//
// 返回的两组标识都保持首次出现的顺序并已去重，两者都可能为空（是否算「没配」由调用方判断）。
// 这是本仓库**唯一**的解析实现：校验、保存后的展示、匹配三处必须共用它，
// 各自再写一份的话，三处口径迟早漂移（历史 Bug 就是这么来的）。
func ParseReconciliationExternalKeys(raw string) (names []string, tokenIDs []int64) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	seenNames := make(map[string]struct{}, 2)
	seenIDs := make(map[int64]struct{}, 2)
	for _, part := range strings.Split(raw, reconciliationExternalKeySeparator) {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if tokenID, ok := reconciliationParseTokenID(item); ok {
			if _, exists := seenIDs[tokenID]; !exists {
				seenIDs[tokenID] = struct{}{}
				tokenIDs = append(tokenIDs, tokenID)
			}
			continue
		}
		if _, exists := seenNames[item]; !exists {
			seenNames[item] = struct{}{}
			names = append(names, item)
		}
	}
	return names, tokenIDs
}

// reconciliationParseTokenID 判断一个令牌标识是不是 "id:<正整数>" 形式。
//
// 要求整段数字都能解析且为正数：id: 后面跟小数、负数、16 进制或空串一律不算 ID，
// 由调用方按普通令牌名处理。
func reconciliationParseTokenID(item string) (int64, bool) {
	if len(item) <= len(reconciliationExternalKeyIDPrefix) {
		return 0, false
	}
	if !strings.EqualFold(item[:len(reconciliationExternalKeyIDPrefix)], reconciliationExternalKeyIDPrefix) {
		return 0, false
	}
	tokenID, err := strconv.ParseInt(strings.TrimSpace(item[len(reconciliationExternalKeyIDPrefix):]), 10, 64)
	if err != nil || tokenID <= 0 {
		return 0, false
	}
	return tokenID, true
}

// reconciliationFormatTokenID 把 ID 还原成配置里的规范写法，供展示用。
func reconciliationFormatTokenID(tokenID int64) string {
	return reconciliationExternalKeyIDPrefix + strconv.FormatInt(tokenID, 10)
}

// ReconciliationAccountRuleView 是规则页的一行。
//
// 它按「账号」而不是「规则」组织：没有规则的账号同样会出现，这样管理员才能在
// 面板上为任何一个账号补规则。列表永远不空，避免出现无从下手的空页面。
//
// GroupChannelCount 是该分组的真实渠道（账号）数，与主站分组页 account_count 同口径
// （只算未软删账号）。它不等于「本表格里该分组的行数」：一行是一个账号，而账号可以同时
// 属于多个分组、这里只展示它优先级最高的那一个，所以同一个分组出现在几行上、以及每个
// 分组到底有几个渠道，是两件不同的事。
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

	GroupID           int64
	GroupName         string
	GroupPriority     int
	GroupChannelCount int64

	AccountName        string
	AccountPlatform    string
	AccountStatus      string
	AccountSchedulable bool

	UsageCount int64
	FirstSeen  *time.Time
	LastSeen   *time.Time

	// RecentModel / RecentGroup* 来自该账号**全历史最后一次调用**，不受筛选窗口影响。
	//
	// 「最近模型」是管理员登记上游令牌名时最重要的线索（文档 19：最近分组 / 最近模型
	// 读取该账号全历史最后一次调用）。它跟 UsageCount / FirstSeen / LastSeen 的口径
	// 刻意不同：后三个是「范围内」的统计，Recent* 是账号身份信息，窗口抹不掉。
	RecentModel     string
	RecentGroupID   int64
	RecentGroupName string
}

// ReconciliationAccountRuleGroupView 是规则页的「分组」视角：一个业务分组 + 它名下的账号。
//
// 为什么要有它：一个分组下可能挂多个账号，而每个账号对应不同的上游令牌名
// （实测：分组 #10 下的账号 #47 用 openai-0.1折、账号 #44 用 openai-1折）。
// 管理员是按「分组」这个业务单位来理解和配置对账的，一行一个账号会把
// 「这个分组到底配齐了没有」拆散在若干行里，谁也一眼看不出来。
//
// 它不替代 Items：Items 仍是逐账号的权威列表（前端旧代码与既有测试依赖它），
// 本结构只是同一批数据按分组再聚合一次，两者由同一次 List 产出，不会互相漂移。
type ReconciliationAccountRuleGroupView struct {
	// GroupID 为 0 表示这个分组里的账号不属于任何分组（见 buildReconciliationRuleGroups）。
	GroupID       int64
	GroupName     string
	GroupPriority int
	// GroupChannelCount 是该分组的真实渠道数，与 Items 同口径（只算未软删账号）；
	// 不属于任何分组的特殊分组恒为 0。
	GroupChannelCount int64

	// Accounts 是该分组下的账号，沿用与 Items 完全相同的行结构与排序规则。
	Accounts []ReconciliationAccountRuleView

	// TokenKeys 是该分组下所有账号已配置的令牌标识，已展开成单个标识（逗号拆开、
	// id: 统一成小写规范写法）、去重并按字典序排序。
	//
	// 排序是为了让「同一集合、不同书写顺序」得到完全一样的输出：前端可以据此做
	// 差异对比，测试也能直接断言相等而不必先排序。
	TokenKeys []string

	// Configured 为 true 表示该分组下**所有**账号都已配置规则。
	// 只要有一个账号没配，整个分组就还不算配齐——这正是分组视角要回答的问题。
	Configured bool

	// UsageCount 是分组内账号的用量合计（窗口内调用数）。
	UsageCount int64
}

// ReconciliationAccountRuleList 是规则页的完整响应载荷。
type ReconciliationAccountRuleList struct {
	From                 time.Time
	To                   time.Time
	UnconfiguredAccounts int64
	// UnconfiguredGroups 是「组内还有账号没配规则」的分组数。
	//
	// 它与 UnconfiguredAccounts 并存而不是替换：账号数说明总工作量，且会随
	// 分组内账号数膨胀；分组数才是管理员要逐个清掉的业务待办条数。
	UnconfiguredGroups int64
	Items              []ReconciliationAccountRuleView
	// Groups 是 Items 按分组聚合后的视图，供「按分组组织」的界面使用。
	//
	// 它与 Items 是同一次查询产出的两份视角，刻意不合并：
	// 前者回答「这个业务分组配齐了吗」，后者回答「这个账号配了什么」。
	Groups []ReconciliationAccountRuleGroupView
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
//
// externalKey 放宽为「一个或多个令牌标识」（英文逗号分隔，允许 id:<数字> 形式，
// 见 ParseReconciliationExternalKeys）。放宽的原因有两个，都是线上实测出来的：
// 上游会改令牌名（glm-3.5-95% → glm），以及一个分组下多个账号各对应不同的上游令牌名，
// 管理员需要把「同一个令牌的旧名、新名、稳定 ID」写在同一行里。
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
	// 整串至少要能拆出一个可用标识，否则就是一条「看起来配好了、实际永远匹配不上」的规则：
	// 例如只填了 ","（或 " , , "），页面会把它显示成已配置，对账却全挂。
	// 这种输入比空串更危险，因此沿用「缺少令牌标识」这个既有错误码直接拒掉，不新增错误码。
	if names, tokenIDs := ParseReconciliationExternalKeys(key); len(names) == 0 && len(tokenIDs) == 0 {
		return "", ErrReconciliationTokenNameRequired
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

	// 最近模型 / 最近分组走全历史查询，与窗口内的用量统计分开取：
	// 一次 DISTINCT ON 查询覆盖全部账号，比逐账号查「最后一次调用」少 N 次往返。
	recentByAccount, err := s.ledgerSvc.RecentUsageByAccount(ctx)
	if err != nil {
		return nil, err
	}

	// 分组的真实渠道数单独统计：它回答的是「这个分组一共挂了几个渠道」，
	// 不是「这个分组在本表格里占了几行」——后者会少算（账号只能展示一个分组）。
	groupChannelCounts, err := s.ruleRepo.CountAccountsByGroup(ctx)
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
		view.GroupChannelCount = groupChannelCounts[view.GroupID]

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
			firstSeen, lastSeen := usage.FirstSeen, usage.LastSeen
			view.FirstSeen = &firstSeen
			view.LastSeen = &lastSeen
			if !view.Configured {
				unconfigured++
			}
		}

		// 最近一次调用的信息独立赋值：账号可能在本窗口内没有任何调用，
		// 但历史上跑过——那种账号更要显示最近模型/分组，它正是「给不给它配规则」的依据。
		if recent, ok := recentByAccount[account.ID]; ok {
			view.RecentModel = recent.Model
			view.RecentGroupID = recent.GroupID
			view.RecentGroupName = recent.GroupName
		}

		items = append(items, view)
	}

	// 有调用的账号排在前面，方便管理员优先处理真正在跑流量的账号；
	// 同组内按调用量降序，再按账号 ID 升序保证顺序稳定。
	//
	// 比较函数抽成具名函数：分组视图里的「组内账号」用同一份实现，
	// 两处各写一遍的话，迟早出现「Items 的顺序与展开分组后的顺序不一致」这种灵异现象。
	sort.SliceStable(items, func(i, j int) bool {
		return reconciliationRuleViewLess(items[i], items[j])
	})

	groups := buildReconciliationRuleGroups(items)
	var unconfiguredGroups int64
	for i := range groups {
		// 与 UnconfiguredAccounts 同口径：只数「有调用但没配好」的分组。
		if reconciliationGroupNeedsRule(&groups[i]) {
			unconfiguredGroups++
		}
	}

	return &ReconciliationAccountRuleList{
		From:                 from,
		To:                   to,
		UnconfiguredAccounts: unconfigured,
		UnconfiguredGroups:   unconfiguredGroups,
		Items:                items,
		Groups:               groups,
	}, nil
}

// reconciliationRuleViewLess 是规则页行的排序规则：有用量优先 → 用量降序 → 账号 ID 升序。
//
// 「有用量优先」与「用量降序」分开写不是啰嗦：前者让从来没用过的账号沉底
// （它们的 UsageCount 都是 0，光靠降序会与「用了 0 次」混在一起），
// 后者再在同一档里排出先后。
func reconciliationRuleViewLess(a, b ReconciliationAccountRuleView) bool {
	if (a.UsageCount > 0) != (b.UsageCount > 0) {
		return a.UsageCount > 0
	}
	if a.UsageCount != b.UsageCount {
		return a.UsageCount > b.UsageCount
	}
	return a.AccountID < b.AccountID
}

// buildReconciliationRuleGroups 把逐账号的规则视图按分组再聚合一次。
//
// 分组归属**直接沿用**每行已经算好的 GroupID/GroupName/GroupPriority（来自
// reconcileAccountGroup），这里不重新判断归属：两份归属逻辑一定会漂移，
// 而「一行一个账号」的归属口径已经被主站分组页与既有测试锁定。
//
// 没有归任何分组的账号（GroupID == 0）单独聚成一个 GroupID=0、GroupName 为空串的特殊分组：
// 管理员需要看见「这些账号不属于任何分组」这个事实，把它们藏起来会让分组视角的账号总数
// 与 Items 对不上，而这种对不上最难排查。
func buildReconciliationRuleGroups(items []ReconciliationAccountRuleView) []ReconciliationAccountRuleGroupView {
	// 用下标而不是指针索引：切片 append 可能重新分配底层数组，
	// 之前取到的元素指针会指向旧数组，后续写入静默丢失（经典的踩坑点）。
	indexByGroup := make(map[int64]int, 8)
	groups := make([]ReconciliationAccountRuleGroupView, 0, 8)

	for i := range items {
		view := items[i]
		groupIndex, exists := indexByGroup[view.GroupID]
		if !exists {
			groups = append(groups, ReconciliationAccountRuleGroupView{
				GroupID:   view.GroupID,
				GroupName: view.GroupName,
				// GroupPriority 取组内成员的展示优先级：同组账号的这个值来自同一个分组，
				// 本来就是同一个数，这里只是把它带到分组行上供前端渲染。
				GroupPriority: view.GroupPriority,
				// 分组的真实渠道数沿用 Items 的口径（GroupID 为 0 时分组表里没有这一行，
				// 查表自然得 0，正是「不属于任何分组」应有的取值）。
				GroupChannelCount: view.GroupChannelCount,
				// 空切片而不是 nil：序列化成 [] 而不是 null，前端不必额外判空。
				Accounts:  make([]ReconciliationAccountRuleView, 0, 4),
				TokenKeys: make([]string, 0, 4),
			})
			groupIndex = len(groups) - 1
			indexByGroup[view.GroupID] = groupIndex
		}

		group := &groups[groupIndex]
		group.Accounts = append(group.Accounts, view)
		group.UsageCount += view.UsageCount
		group.TokenKeys = append(group.TokenKeys, reconciliationViewTokenKeys(&view)...)
	}

	for i := range groups {
		group := &groups[i]
		// 组内账号沿用与 Items 完全相同的排序规则，保证「展开分组」和「看总表」顺序一致。
		sort.SliceStable(group.Accounts, func(a, b int) bool {
			return reconciliationRuleViewLess(group.Accounts[a], group.Accounts[b])
		})
		group.TokenKeys = reconciliationSortedUniqueKeys(group.TokenKeys)

		// 组内所有账号都已配置才算这个分组配齐。空组不存在（分组由账号聚合而来），
		// 因此不需要为「没有账号」单独讨论。
		group.Configured = true
		for a := range group.Accounts {
			if !group.Accounts[a].Configured {
				group.Configured = false
				break
			}
		}
	}

	// 分组之间与账号之间同规则：有用量优先 → 分组内用量合计降序 → GroupID 升序。
	// 用 SliceStable，保证同一用量下的分组顺序不随 map 遍历顺序抖动。
	sort.SliceStable(groups, func(i, j int) bool {
		if (groups[i].UsageCount > 0) != (groups[j].UsageCount > 0) {
			return groups[i].UsageCount > 0
		}
		if groups[i].UsageCount != groups[j].UsageCount {
			return groups[i].UsageCount > groups[j].UsageCount
		}
		return groups[i].GroupID < groups[j].GroupID
	})

	return groups
}

// reconciliationGroupNeedsRule 判断一个分组是否算「有调用待配置」。
//
// 口径与 UnconfiguredAccounts 完全一致，只是把统计单位从账号换成分组：
// 组内只要有一个**窗口内有调用**且没配好规则的账号，这个分组就进了管理员的待办清单。
//
// 注意它与 ReconciliationAccountRuleGroupView.Configured 回答的不是同一个问题：
// Configured 是「组内全部账号都配好了吗」（更严格，一个从没调用的账号没配规则也会让它为 false）。
// 两者刻意不同——前端表头显示的是「有调用待配置 N 个分组」，
// 把零调用的分组也算进去会让这个数字与它的文案（以及管理员真正要处理的量）对不上。
func reconciliationGroupNeedsRule(group *ReconciliationAccountRuleGroupView) bool {
	if group == nil {
		return false
	}
	for i := range group.Accounts {
		if group.Accounts[i].UsageCount > 0 && !group.Accounts[i].Configured {
			return true
		}
	}
	return false
}

// reconciliationViewTokenKeys 把一个账号已配置的 external_key 展开成分组视角下的令牌标识。
//
// 只展开**已配置**账号的键：没配规则的账号本来就没有标识，硬塞一个空串
// 会让分组视图看起来「配了个空令牌」。
func reconciliationViewTokenKeys(view *ReconciliationAccountRuleView) []string {
	if view == nil || !view.Configured {
		return nil
	}
	names, tokenIDs := ParseReconciliationExternalKeys(view.ExternalKey)
	keys := make([]string, 0, len(names)+len(tokenIDs))
	keys = append(keys, names...)
	for _, tokenID := range tokenIDs {
		// 规范写成小写 "id:NN"：管理员可能写成 "ID: 41210" 或 "id:41210"，
		// 展开后统一成一种写法，前端做差集/比对时不必再归一化。
		keys = append(keys, reconciliationFormatTokenID(tokenID))
	}
	return keys
}

// reconciliationSortedUniqueKeys 去重并按字典序排序。
//
// 排序而不是保留出现顺序：同一个集合无论账号书写顺序如何，输出必须完全一致，
// 否则分组卡片上的令牌列表会随账号顺序变化而跳动，前端 diff 也会一直报差异。
func reconciliationSortedUniqueKeys(keys []string) []string {
	if len(keys) == 0 {
		// 返回空切片而不是 nil：契约里这是数组，不是 null。
		return []string{}
	}
	seen := make(map[string]struct{}, len(keys))
	unique := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	sort.Strings(unique)
	return unique
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
