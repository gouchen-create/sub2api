package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/shopspring/decimal"
)

// 匹配方式，写入 reconciliation_upstream_bills.match_method。
//
// 四级顺序不可调换：先试最精确的直连匹配，再逐级放宽。放宽级别都带唯一性要求，
// 因为「同令牌有账单」不等于「账单属于这条请求」——同一个上游令牌可能被多个站点共用，
// 账单数天然可能少于本站调用数。宁可留「上游待匹配」，也不错配。
const (
	ReconciliationMatchDirectRequestID         = "direct_request_id"
	ReconciliationMatchCompositeTokensTime     = "composite_account_model_tokens_time"
	ReconciliationMatchCompositeCacheRead      = "composite_account_model_tokens_time_cache_read"
	ReconciliationMatchCompositeCacheTolerance = "composite_account_model_tokens_time_cache_tolerance"
)

// 同步状态键。
const (
	ReconciliationStateKeyUsageCursor       = "usage_last_collected_at"
	ReconciliationStateKeyA6LastSyncUnix    = "a6_last_sync_unix"
	ReconciliationStateKeyA6LastSyncError   = "a6_last_sync_error"
	ReconciliationStateKeyA6LastSyncErrorAt = "a6_last_sync_error_at"
	ReconciliationStateKeyA6BootstrapPrefix = "a6_bootstrap_done:"
	ReconciliationStateKeyBackfillStatus    = "a6_backfill_status"
	ReconciliationStateKeyBackfillFrom      = "a6_backfill_from"
	ReconciliationStateKeyBackfillTo        = "a6_backfill_to"
	ReconciliationStateKeyBackfillCursor    = "a6_backfill_cursor"
	ReconciliationStateKeyBackfillProcessed = "a6_backfill_processed"
	ReconciliationStateKeyBackfillError     = "a6_backfill_error"
)

// 回填状态取值。前端契约文档写的是 running / done / failed（历史实现写成 completed，
// 属于文档与实现漂移，这里以契约为准）。
const (
	ReconciliationBackfillStatusRunning = "running"
	ReconciliationBackfillStatusDone    = "done"
	ReconciliationBackfillStatusFailed  = "failed"
)

// 匹配窗口与容差。
const (
	// reconciliationCompositeWindow 组合匹配允许的时间偏差。
	reconciliationCompositeWindow = 2 * time.Minute
	// reconciliationCacheToleranceWindow 缓存相差 1 的窄兜底允许的时间偏差。
	reconciliationCacheToleranceWindow = 2 * time.Second
	// reconciliationCollectOverlap 采集游标回退量，避免边界上的调用被漏掉。
	// 重叠部分靠快照表的唯一索引幂等去重，重复扫描没有副作用。
	reconciliationCollectOverlap = time.Minute
)

// 采集与匹配错误。
var (
	// ErrReconciliationBillSourceUnavailable 表示尚未配置上游账单来源。
	ErrReconciliationBillSourceUnavailable = errors.New("RECONCILIATION_BILL_SOURCE_UNAVAILABLE")
)

// ReconciliationSyncConfig 是采集与匹配的可调参数。
type ReconciliationSyncConfig struct {
	// FxUSDCNYRate 美元转人民币的换算数字。填 1 等价于不做换算。
	FxUSDCNYRate float64
	// A6Lookback 首次同步的回看窗口。
	A6Lookback time.Duration
	// CollectBatchSize 单次采集的调用条数上限。
	CollectBatchSize int
	// StagingLimit 单轮匹配处理的账单条数上限。
	StagingLimit int
	// MatchGracePeriod 账单导入后等待多久才判定为「匹配不上」。
	//
	// 这个宽限期是必要的：上游账单往往比本站用量更早落库，若立刻判定为孤儿，
	// 看板上会先冒出一批「上游待匹配」，几十秒后又自己变成「已对账」，非常惊悚。
	MatchGracePeriod time.Duration
}

// ReconciliationSyncService 负责下游用量采集、上游账单导入与两者匹配。
type ReconciliationSyncService struct {
	extrasRepo  ReconciliationUsageExtraRepository
	billRepo    ReconciliationUpstreamBillRepository
	ruleRepo    ReconciliationAccountRuleRepository
	stateRepo   ReconciliationSyncStateRepository
	usageSource ReconciliationUsageSource
	billSource  ReconciliationUpstreamBillSource
	cfg         ReconciliationSyncConfig

	// triggerMu 保证同一进程内不会并发跑两轮采集：手动点「立即采集」与定时任务
	// 撞在一起时，第二轮直接跳过而不是排队，避免对上游造成重复压力。
	triggerMu sync.Mutex
}

// NewReconciliationSyncService 创建对账同步服务。
//
// billSource 允许为 nil：此时采集与匹配仍然可用（只处理手动导入的账单与本地用量），
// 只是不会主动去上游拉账单。这样即使上游凭据还没配好，看板与规则页依然能用。
func NewReconciliationSyncService(
	extrasRepo ReconciliationUsageExtraRepository,
	billRepo ReconciliationUpstreamBillRepository,
	ruleRepo ReconciliationAccountRuleRepository,
	stateRepo ReconciliationSyncStateRepository,
	usageSource ReconciliationUsageSource,
	billSource ReconciliationUpstreamBillSource,
	cfg ReconciliationSyncConfig,
) *ReconciliationSyncService {
	// 汇率的「<= 0 就当成 1」与设置服务共用同一个归一化函数，避免两边口径漂移。
	cfg.FxUSDCNYRate = normalizeReconciliationFxDefault(cfg.FxUSDCNYRate)
	if cfg.CollectBatchSize <= 0 {
		cfg.CollectBatchSize = 5000
	}
	if cfg.StagingLimit <= 0 {
		cfg.StagingLimit = 1000
	}
	if cfg.MatchGracePeriod <= 0 {
		cfg.MatchGracePeriod = 30 * time.Minute
	}
	return &ReconciliationSyncService{
		extrasRepo:  extrasRepo,
		billRepo:    billRepo,
		ruleRepo:    ruleRepo,
		stateRepo:   stateRepo,
		usageSource: usageSource,
		billSource:  billSource,
		cfg:         cfg,
	}
}

// FxRate 返回当前生效的换算数字，供接口层展示。
func (s *ReconciliationSyncService) FxRate() float64 {
	return s.cfg.FxUSDCNYRate
}

// CollectUsage 把新产生的调用采集进快照表，返回新增条数。
//
// 快照里冻结两样东西：调用发生时该账号对应的上游令牌，以及采集时的换算数字。
// 冻结令牌是为了令牌改名后旧账单仍能匹配（旧实现只读当前映射，令牌一改名
// 历史账单全部变成孤儿，实测影响 1157 条）；冻结汇率是为了历史金额不随后续
// 汇率调整而漂移。
func (s *ReconciliationSyncService) CollectUsage(ctx context.Context) (int64, error) {
	now := time.Now().UTC()

	since, err := s.resolveUsageCursor(ctx, now)
	if err != nil {
		return 0, err
	}

	facts, err := s.usageSource.ListUsageBetween(ctx, since, now, s.cfg.CollectBatchSize)
	if err != nil {
		return 0, err
	}
	if len(facts) == 0 {
		return 0, nil
	}

	rules, err := s.ruleIndexByAccount(ctx)
	if err != nil {
		return 0, err
	}

	effectiveRate := s.EffectiveFxRate(ctx)
	fxRate := decimal.NewFromFloat(effectiveRate)
	extras := make([]ReconciliationUsageExtra, 0, len(facts))
	for _, fact := range facts {
		rule, hasRule := rules[fact.AccountID]

		extra := ReconciliationUsageExtra{
			UsageLogID:      fact.UsageLogID,
			AccountID:       fact.AccountID,
			RevenueOriginal: fact.ActualCost,
			FxRateToCNY:     effectiveRate,
			RevenueCNY:      decimal.NewFromFloat(fact.ActualCost).Mul(fxRate).InexactFloat64(),
			CollectedAt:     now,
		}
		if hasRule && rule.Enabled {
			extra.RuleProvider = rule.Provider
			extra.RuleExternalKey = rule.ExternalKey
			extra.RuleVersion = rule.Version
		}
		extras = append(extras, extra)
	}

	inserted, err := s.extrasRepo.UpsertBatch(ctx, extras)
	if err != nil {
		return inserted, err
	}

	// 游标只在写入成功后推进；失败时下一轮会重扫同一区间，
	// 由快照表的唯一索引保证不会产生重复行。
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyUsageCursor, now.Format(time.RFC3339Nano)); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "usage_cursor_advance_failed: err=%v", err)
	}
	return inserted, nil
}

// resolveUsageCursor 解析采集起点。
//
// 首次运行时回看 A6Lookback，之后从上次成功位置略微回退一点，
// 覆盖时钟抖动与迟到落库的调用。
func (s *ReconciliationSyncService) resolveUsageCursor(ctx context.Context, now time.Time) (time.Time, error) {
	raw, err := s.stateRepo.Get(ctx, ReconciliationStateKeyUsageCursor)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) == "" {
		lookback := s.cfg.A6Lookback
		if lookback <= 0 {
			lookback = 24 * time.Hour
		}
		return now.Add(-lookback), nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "usage_cursor_unparsable: raw=%q err=%v", raw, err)
		lookback := s.cfg.A6Lookback
		if lookback <= 0 {
			lookback = 24 * time.Hour
		}
		return now.Add(-lookback), nil
	}
	return parsed.Add(-reconciliationCollectOverlap), nil
}

// ruleIndexByAccount 把全部规则按账号索引起来。
func (s *ReconciliationSyncService) ruleIndexByAccount(ctx context.Context) (map[int64]ReconciliationAccountRule, error) {
	rules, err := s.ruleRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[int64]ReconciliationAccountRule, len(rules))
	for _, rule := range rules {
		index[rule.AccountID] = rule
	}
	return index, nil
}

// SyncA6Bills 从上游拉取账单并幂等导入，返回新增条数。
func (s *ReconciliationSyncService) SyncA6Bills(ctx context.Context, from, to time.Time) (int64, error) {
	if s.billSource == nil {
		return 0, ErrReconciliationBillSourceUnavailable
	}

	bills, err := s.billSource.FetchBills(ctx, ReconciliationBillQuery{
		From:     from,
		To:       to,
		PageSize: s.cfg.StagingLimit,
	})
	if err != nil {
		s.recordSyncError(ctx, err)
		return 0, err
	}

	// 导入时把换算数字抄到每条账单上，落库即冻结。
	fxRate := s.EffectiveFxRate(ctx)
	for i := range bills {
		if bills[i].FxRateToCNY <= 0 {
			bills[i].FxRateToCNY = fxRate
		}
		bills[i].CostCNY = decimal.NewFromFloat(bills[i].CostOriginal).
			Mul(decimal.NewFromFloat(bills[i].FxRateToCNY)).
			InexactFloat64()
	}

	inserted, err := s.billRepo.UpsertBatch(ctx, bills)
	if err != nil {
		s.recordSyncError(ctx, err)
		return inserted, err
	}

	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyA6LastSyncUnix, strconv.FormatInt(time.Now().UTC().Unix(), 10)); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "a6_sync_state_write_failed: err=%v", err)
	}
	// 本轮成功就把上次的错误清掉，避免旧错误长期挂在页面上误导排查。
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyA6LastSyncError, ""); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "a6_sync_error_clear_failed: err=%v", err)
	}
	return inserted, nil
}

func (s *ReconciliationSyncService) recordSyncError(ctx context.Context, cause error) {
	if cause == nil {
		return
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyA6LastSyncError, cause.Error()); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "a6_sync_error_write_failed: err=%v", err)
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyA6LastSyncErrorAt, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "a6_sync_error_time_write_failed: err=%v", err)
	}
}

// MatchStaging 给尚未匹配的账单寻找对应的本站调用。
//
// 返回 (本轮成功匹配数, 判定为孤儿的账单数, 错误)。
func (s *ReconciliationSyncService) MatchStaging(ctx context.Context, from, to time.Time) (int64, int64, error) {
	bills, err := s.billRepo.ListStaging(ctx, from, to, s.cfg.StagingLimit)
	if err != nil {
		return 0, 0, err
	}
	if len(bills) == 0 {
		return 0, 0, nil
	}

	// 账号解析结果按令牌名缓存，避免同一令牌反复查库。
	accountCache := make(map[string][]int64, 8)
	now := time.Now().UTC()

	var matched int64
	var orphanIDs []int64

	for i := range bills {
		bill := &bills[i]

		accountIDs, ok := accountCache[bill.Payload.TokenName]
		if !ok {
			accountIDs, err = s.resolveAccountIDsForToken(ctx, bill.Payload.TokenName)
			if err != nil {
				return matched, 0, err
			}
			accountCache[bill.Payload.TokenName] = accountIDs
		}

		if len(accountIDs) > 0 {
			candidate, method := s.findMatch(ctx, bill, accountIDs)
			if candidate != nil {
				if err := s.billRepo.MarkMatched(ctx, bill.ID, candidate.UsageLogID, candidate.AccountID, method); err != nil {
					// 竞态：这条调用刚被别的流程匹配走。不视为失败，留给下一轮重试。
					logger.LegacyPrintf("service.reconciliation_sync", "bill_match_race: bill_id=%d err=%v", bill.ID, err)
				} else {
					matched++
					continue
				}
			}
		}

		// 只有过了宽限期仍未匹配上，才认定为孤儿账单。
		if now.Sub(bill.Payload.OccurredAt) >= s.cfg.MatchGracePeriod {
			orphanIDs = append(orphanIDs, bill.ID)
		}
	}

	if len(orphanIDs) > 0 {
		if err := s.billRepo.MarkUnmatched(ctx, orphanIDs); err != nil {
			return matched, 0, err
		}
	}

	return matched, int64(len(orphanIDs)), nil
}

// resolveAccountIDsForToken 找出某个上游令牌可能对应的本站账号。
//
// 查两处，这是修复「令牌改名后历史账单变孤儿」的关键：
//  1. 当前规则表 —— 令牌名仍然在用的账号
//  2. 历史快照表 —— 调用发生时该账号用的是这个名字的账号
//
// 旧实现只查第 1 处，管理员把令牌从 0.12 改名成 0.15-claude 之后，
// 所有旧账单立刻无法回配，线上实测影响 1157 条。
func (s *ReconciliationSyncService) resolveAccountIDsForToken(ctx context.Context, tokenName string) ([]int64, error) {
	tokenName = strings.TrimSpace(tokenName)
	if tokenName == "" {
		return nil, nil
	}

	rules, err := s.ruleRepo.List(ctx)
	if err != nil {
		return nil, err
	}

	seen := make(map[int64]struct{}, 4)
	ids := make([]int64, 0, 4)
	for _, rule := range rules {
		if !rule.Enabled || rule.ExternalKey != tokenName {
			continue
		}
		if _, exists := seen[rule.AccountID]; exists {
			continue
		}
		seen[rule.AccountID] = struct{}{}
		ids = append(ids, rule.AccountID)
	}

	historical, err := s.extrasRepo.ListAccountIDsByRuleKeys(ctx, []string{tokenName})
	if err != nil {
		return nil, err
	}
	for _, accountID := range historical {
		if _, exists := seen[accountID]; exists {
			continue
		}
		seen[accountID] = struct{}{}
		ids = append(ids, accountID)
	}

	return ids, nil
}

// findMatch 按四级顺序为一条账单寻找唯一对应的调用，返回命中的候选与匹配方式。
func (s *ReconciliationSyncService) findMatch(ctx context.Context, bill *ReconciliationUpstreamBill, accountIDs []int64) (*ReconciliationMatchCandidate, string) {
	payload := &bill.Payload

	// 第 1 级：上游请求 ID 直连。这是唯一「确定」的匹配，命中即用。
	//
	// 要求候选唯一：若多个调用声称同一个上游请求 ID（例如上游重放了标识），
	// 说明这个标识不具备区分度，退回组合匹配而不是随便挑一个。
	if upstreamID := strings.TrimSpace(payload.UpstreamRequestID); upstreamID != "" {
		candidates, err := s.billRepo.FindDirectMatchCandidates(ctx, upstreamID)
		if err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "direct_match_query_failed: err=%v", err)
		} else if len(candidates) == 1 {
			return &candidates[0], ReconciliationMatchDirectRequestID
		}
	}

	// 第 2/3/4 级共用一次粗筛：账号范围 + 模型 + 输出 token + 时间窗口。
	candidates, err := s.billRepo.FindCompositeMatchCandidates(ctx, ReconciliationCompositeQuery{
		AccountIDs:   accountIDs,
		Model:        payload.Model,
		OutputTokens: payload.OutputTokens,
		OccurredAt:   payload.OccurredAt,
		TimeWindow:   reconciliationCompositeWindow,
	})
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "composite_match_query_failed: err=%v", err)
		return nil, ""
	}
	if len(candidates) == 0 {
		return nil, ""
	}

	// 第 2 级：输入与缓存 token 全部一致。
	if candidate := pickUniqueClosest(filterExactTokenMatches(candidates, payload), payload.OccurredAt); candidate != nil {
		return candidate, ReconciliationMatchCompositeTokensTime
	}

	// 第 3 级：只比缓存读取，忽略缓存写入的差异。
	if candidate := pickUniqueClosest(filterCacheReadMatches(candidates, payload), payload.OccurredAt); candidate != nil {
		return candidate, ReconciliationMatchCompositeCacheRead
	}

	// 第 4 级：缓存 token 恰好相差 1 的窄兜底，附加严格闸门。
	if candidate := s.matchCacheTolerance(candidates, payload); candidate != nil {
		return candidate, ReconciliationMatchCompositeCacheTolerance
	}

	return nil, ""
}

// matchCacheTolerance 处理缓存 token 恰好相差 1 的情况。
//
// 闸门全部满足才认，缺一不可：缓存数必须大于 0、时间差不超过 2 秒、
// 候选严格只有 1 个。缓存相差 1 本身是个很弱的信号，放宽任何一条都会开始错配。
func (s *ReconciliationSyncService) matchCacheTolerance(candidates []ReconciliationMatchCandidate, payload *ReconciliationUpstreamBillPayload) *ReconciliationMatchCandidate {
	billCache := billCacheTotal(payload)
	if billCache <= 0 {
		return nil
	}

	tolerant := make([]ReconciliationMatchCandidate, 0, 2)
	for _, candidate := range candidates {
		if absInt(candidateCacheTotal(candidate)-billCache) != 1 {
			continue
		}
		if absDuration(candidate.CreatedAt.Sub(payload.OccurredAt)) > reconciliationCacheToleranceWindow {
			continue
		}
		tolerant = append(tolerant, candidate)
	}
	if len(tolerant) != 1 {
		return nil
	}
	return &tolerant[0]
}

// filterExactTokenMatches 保留缓存合计与输入 token 都与账单一致的候选。
//
// 缓存一律比「合计」而不是比读写分列：上游可能只回一个合并值（other.cache_tokens），
// 此时两个分列字段都是 0，拿 0 去比对会让这类账单全部匹配失败。
//
// 输入 token 接受两种等价写法：上游可能只算纯输入，也可能把缓存并入输入。
// 两种口径在实际账单里都出现过，只认一种会漏掉大量本可对上的记录。
func filterExactTokenMatches(candidates []ReconciliationMatchCandidate, payload *ReconciliationUpstreamBillPayload) []ReconciliationMatchCandidate {
	billCache := billCacheTotal(payload)
	result := make([]ReconciliationMatchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidateCacheTotal(candidate) != billCache {
			continue
		}
		if !inputTokensEquivalent(candidate.InputTokens, payload.InputTokens, billCache) {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

// filterCacheReadMatches 保留缓存读取一致、输入 token 等价的候选。
//
// 放宽的是缓存写入：上游与本站对缓存写入的记账时机不同，这一项差异最常见，
// 但它对「是不是同一次请求」的判别力远弱于缓存读取。
//
// 上游只回合并值时没有独立的「读」值，第 3 级无法与第 2 级区分，直接返回空，
// 让判定落到第 4 级的容差兜底，而不是拿 0 去比出一个假的相等。
func filterCacheReadMatches(candidates []ReconciliationMatchCandidate, payload *ReconciliationUpstreamBillPayload) []ReconciliationMatchCandidate {
	if payload.CacheReadTokens <= 0 {
		return nil
	}
	billCache := billCacheTotal(payload)
	result := make([]ReconciliationMatchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.CacheReadTokens != payload.CacheReadTokens {
			continue
		}
		if !inputTokensEquivalent(candidate.InputTokens, payload.InputTokens, billCache) {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

// billCacheTotal 返回上游账单口径的缓存合计。
//
// 分列值齐全时合计就是两者之和；上游只回合并值时合计直接取该值。
func billCacheTotal(payload *ReconciliationUpstreamBillPayload) int {
	if payload.CacheTokensTotal > 0 {
		return payload.CacheTokensTotal
	}
	return payload.CacheReadTokens + payload.CacheCreationTokens
}

// candidateCacheTotal 返回本站调用记录的缓存合计。
func candidateCacheTotal(candidate ReconciliationMatchCandidate) int {
	return candidate.CacheReadTokens + candidate.CacheCreationTok
}

// inputTokensEquivalent 判断本站记录的输入 token 与上游账单是否等价。
func inputTokensEquivalent(candidateInput, billInput, billCacheRead int) bool {
	if candidateInput == billInput {
		return true
	}
	// 上游把缓存读取并入输入 token 的写法。
	return candidateInput == billInput+billCacheRead
}

// pickUniqueClosest 从候选里挑时间差最小的那一个。
//
// 最小时间差并列时返回 nil：并列意味着无法唯一确定是哪一次调用，
// 猜错会把成本记到别的请求上，比留一条「待对账」严重得多。
func pickUniqueClosest(candidates []ReconciliationMatchCandidate, occurredAt time.Time) *ReconciliationMatchCandidate {
	if len(candidates) == 0 {
		return nil
	}

	bestIndex := -1
	var bestDelta time.Duration
	tied := false
	for i := range candidates {
		delta := absDuration(candidates[i].CreatedAt.Sub(occurredAt))
		switch {
		case bestIndex < 0 || delta < bestDelta:
			bestIndex, bestDelta, tied = i, delta, false
		case delta == bestDelta:
			tied = true
		}
	}
	if bestIndex < 0 || tied {
		return nil
	}
	return &candidates[bestIndex]
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// RunOnce 执行一轮完整的采集：下游用量 -> 上游账单 -> 匹配。
//
// 三个阶段各自独立容错：上游拉取失败不会阻止本地用量采集与已有账单的匹配，
// 这样即使上游临时不可用，看板上的本地数据仍然是最新的。
func (s *ReconciliationSyncService) RunOnce(ctx context.Context) error {
	if _, err := s.CollectUsage(ctx); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "collect_usage_failed: err=%v", err)
	}

	now := time.Now().UTC()
	lookback := s.cfg.A6Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	from := now.Add(-lookback)

	if s.billSource != nil {
		if _, err := s.SyncA6Bills(ctx, from, now); err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "sync_a6_bills_failed: err=%v", err)
		}
	}

	if _, _, err := s.MatchStaging(ctx, from, now); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "match_staging_failed: err=%v", err)
	}
	return nil
}

// TriggerAsync 在后台触发一轮采集并立即返回。
//
// 接口层的 POST /collect 必须立刻返回：前端 HTTP 超时是 30 秒，而上游单次
// 拉取最长可能到 90 秒，同步等待必然超时。这里用 TryLock 语义，
// 已有采集在跑时直接跳过，不排队堆积。
func (s *ReconciliationSyncService) TriggerAsync(ctx context.Context) bool {
	if !s.triggerMu.TryLock() {
		return false
	}
	// 脱离请求生命周期：请求返回后 ctx 会被取消，采集必须继续跑完。
	runCtx := context.WithoutCancel(ctx)
	go func() {
		defer s.triggerMu.Unlock()
		s.RunOnce(runCtx)
	}()
	return true
}

// SyncStatus 汇总同步状态，供 /status 与页面提示使用。
type ReconciliationSyncStatus struct {
	Enabled         bool
	Healthy         bool
	LastSyncAt      *time.Time
	LastError       string
	LastErrorAt     *time.Time
	FxRate          float64
	BillSourceReady bool
}

// Status 读取当前同步状态。
func (s *ReconciliationSyncService) Status(ctx context.Context) (*ReconciliationSyncStatus, error) {
	keys := []string{
		ReconciliationStateKeyA6LastSyncUnix,
		ReconciliationStateKeyA6LastSyncError,
		ReconciliationStateKeyA6LastSyncErrorAt,
	}
	values, err := s.stateRepo.GetMultiple(ctx, keys)
	if err != nil {
		return nil, err
	}

	status := &ReconciliationSyncStatus{
		Enabled:         true,
		Healthy:         true,
		FxRate:          s.cfg.FxUSDCNYRate,
		BillSourceReady: s.billSource != nil,
		LastError:       values[ReconciliationStateKeyA6LastSyncError],
	}

	if raw := strings.TrimSpace(values[ReconciliationStateKeyA6LastSyncUnix]); raw != "" {
		if seconds, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			status.LastSyncAt = &at
		}
	}
	if raw := strings.TrimSpace(values[ReconciliationStateKeyA6LastSyncErrorAt]); raw != "" {
		if at, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
			status.LastErrorAt = &at
		}
	}
	// 采集中枢依赖的状态都可读时就算健康；上游凭据缺失单独由 BillSourceReady 表达，
	// 不让整个页面显示为故障——本地对账数据依然可用。
	if status.LastError != "" {
		status.Healthy = false
	}
	return status, nil
}

// DescribeMatchMethod 返回匹配方式的中文说明，供明细行展示。
func DescribeMatchMethod(method string) string {
	switch method {
	case ReconciliationMatchDirectRequestID:
		return "上游请求 ID 直连匹配"
	case ReconciliationMatchCompositeTokensTime:
		return "账号与 token 组合匹配"
	case ReconciliationMatchCompositeCacheRead:
		return "组合匹配（放宽缓存写入）"
	case ReconciliationMatchCompositeCacheTolerance:
		return "组合匹配（缓存容差）"
	default:
		return ""
	}
}

// formatReconciliationAmount 把金额格式化成接口约定的 8 位小数字符串。
func formatReconciliationAmount(value float64) string {
	return decimal.NewFromFloat(value).StringFixed(8)
}
