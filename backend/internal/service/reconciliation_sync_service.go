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
	ReconciliationStateKeyUsageCursorID     = "usage_last_collected_id"
	ReconciliationStateKeyUsageBatchSize    = "usage_last_batch_size"
	ReconciliationStateKeyUsageTruncated    = "usage_last_batch_truncated"
	ReconciliationStateKeyUsageBacklog      = "usage_pending_backlog"
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

// reconciliationBacklogProbeLimit 是积压探针的计数上限。
//
// 积压数字只用于判断「采集器追不上了」，不需要精确值：数到十万行就够了，
// 再往上数只是白烧 IO。返回值等于该上限时含义是「至少还有这么多」。
const reconciliationBacklogProbeLimit = 100_000

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
//
// ⚠️ 游标只允许推进到「本轮确实写进快照的位置」，绝不允许越过没采集到的行。
// 旧实现无条件把游标写成 now，于是被单轮上限截掉的那部分行（created_at 落在
// 下一轮起点 cursor-1min 之前）再也不会被任何一轮扫到：它们的下游收入永久记 0，
// 却照样会被匹配到上游账单，直接算出负毛利。
func (s *ReconciliationSyncService) CollectUsage(ctx context.Context) (int64, error) {
	now := time.Now().UTC()

	cursor, err := s.resolveUsageCursor(ctx, now)
	if err != nil {
		return 0, err
	}

	query := ReconciliationUsageQuery{
		// 起点回退一分钟：重叠部分靠快照表的唯一索引（usage_log_id）幂等去重，
		// 重复扫描没有副作用，但能兜住时钟抖动与迟到落库的调用。
		From:       cursor.At.Add(-reconciliationCollectOverlap),
		To:         now,
		BoundaryAt: cursor.At,
		BoundaryID: cursor.ID,
		Limit:      s.cfg.CollectBatchSize,
	}

	facts, err := s.usageSource.ListUsageBetween(ctx, query)
	if err != nil {
		return 0, err
	}

	// 取满上限 = 窗口内还有没读完的行。这个布尔值决定游标能不能跳到 now，
	// 也决定要不要打告警。
	truncated := len(facts) >= s.cfg.CollectBatchSize

	if len(facts) > 0 {
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
			// 游标只在写入成功后推进；失败时下一轮会重扫同一区间，
			// 由快照表的唯一索引保证不会产生重复行。
			return inserted, err
		}
		next, err := s.advanceUsageCursor(ctx, cursor, facts, now, truncated)
		if err != nil {
			return inserted, err
		}
		s.recordCollectProgress(ctx, next, len(facts), truncated, now)
		return inserted, nil
	}

	// 本轮没有待采集的行：窗口里确实一条都没有，所以可以安全地把游标推到
	// now-重叠量（保留一分钟重扫带），让 usage_last_collected_at 继续按轮前移——
	// 文档 15.1 就是靠它判断采集器活着的，原地不动会被误判成采集器挂了。
	next, err := s.advanceUsageCursor(ctx, cursor, nil, now, false)
	if err != nil {
		return 0, err
	}
	s.recordCollectProgress(ctx, next, 0, false, now)
	return 0, nil
}

// reconciliationUsageCursor 是采集位置的复合游标：时间 + 同一时刻上的主键水位。
type reconciliationUsageCursor struct {
	// At 已采集到的时间位置。
	At time.Time
	// ID 是 created_at == At 上已被排除的最大主键；0 表示该时刻没有排除任何行。
	ID int64
	// Resolved 表示状态表里确实读到了一个可解析的游标；false 表示这是冷启动起点。
	Resolved bool
}

// resolveUsageCursor 解析采集起点。
//
// 首次运行（或游标不可解析）时回看 A6Lookback，之后从上次成功位置略微回退一点，
// 覆盖时钟抖动与迟到落库的调用。
func (s *ReconciliationSyncService) resolveUsageCursor(ctx context.Context, now time.Time) (reconciliationUsageCursor, error) {
	values, err := s.stateRepo.GetMultiple(ctx, []string{
		ReconciliationStateKeyUsageCursor,
		ReconciliationStateKeyUsageCursorID,
	})
	if err != nil {
		return reconciliationUsageCursor{}, err
	}

	raw := strings.TrimSpace(values[ReconciliationStateKeyUsageCursor])
	if raw == "" {
		return reconciliationUsageCursor{At: s.coldStartCursor(now)}, nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "usage_cursor_unparsable: raw=%q err=%v", raw, err)
		return reconciliationUsageCursor{At: s.coldStartCursor(now)}, nil
	}

	cursor := reconciliationUsageCursor{At: parsed, Resolved: true}
	if idRaw := strings.TrimSpace(values[ReconciliationStateKeyUsageCursorID]); idRaw != "" {
		id, parseErr := strconv.ParseInt(idRaw, 10, 64)
		if parseErr != nil {
			// 主键水位读坏了不致命：按 0 处理等于「该时刻一行都没排除」，
			// 下一轮把该时刻的行整批重扫一遍，幂等写入不会产生重复行。
			logger.LegacyPrintf("service.reconciliation_sync", "usage_cursor_id_unparsable: raw=%q err=%v", idRaw, parseErr)
		} else if id > 0 {
			cursor.ID = id
		}
	}
	return cursor, nil
}

// coldStartCursor 是没有任何可用游标时的起点：当前时间往前回看一个窗口。
func (s *ReconciliationSyncService) coldStartCursor(now time.Time) time.Time {
	lookback := s.cfg.A6Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	return now.Add(-lookback)
}

// advanceUsageCursor 计算并落库下一轮的采集位置，返回写入后的游标。
//
// 不变量（写完后必须成立）：
//
//	所有 created_at < At-重叠量 的调用都已经在 reconciliation_usage_extras 里。
//
// 三条推进规则，各自都是这条不变量的保守特例：
//
//  1. 本批被单轮上限截断（len == CollectBatchSize）：只能推进到本批**最后一行**的
//     (created_at, id)。它之前（含同一时刻更小的主键）的行全部在本批里写过了，
//     所以一个都不越过；剩下的行留给下一轮。
//  2. 本批未截断且非空：窗口 [From, To) 内的行已经全部写完，可以推进到
//     max(上一轮位置, now-重叠量)。这里刻意用 now-重叠量 而不是 now：
//     正常路径下它比 now 更保守（多留一分钟重扫带），却仍然让游标按轮前移。
//  3. 本批为空：窗口里一条都没有，同样推进到该位置。
//
// 同一 created_at 上有超过单轮上限的行时（例如批量导入把上万行写成同一个时间戳），
// 规则 1 会先把主键水位推到本批最大 id，下一轮从水位之后继续取——位置在
// (created_at, id) 上严格单调，因此必然前进，不会出现「连续两轮取到完全相同的批次」。
func (s *ReconciliationSyncService) advanceUsageCursor(
	ctx context.Context,
	prev reconciliationUsageCursor,
	facts []ReconciliationUsageFact,
	now time.Time,
	truncated bool,
) (reconciliationUsageCursor, error) {
	next := reconciliationUsageCursor{Resolved: true}

	switch {
	case truncated && len(facts) > 0:
		last := facts[len(facts)-1]
		next.At = last.CreatedAt
		next.ID = last.UsageLogID
	case len(facts) > 0:
		next.At = maxTime(prev.At, now.Add(-reconciliationCollectOverlap))
	default:
		next.At = maxTime(prev.At, now.Add(-reconciliationCollectOverlap))
	}

	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyUsageCursor, next.At.Format(time.RFC3339Nano)); err != nil {
		return next, err
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyUsageCursorID, strconv.FormatInt(next.ID, 10)); err != nil {
		return next, err
	}
	return next, nil
}

// recordCollectProgress 把「本轮取了多少行、有没有被上限截断、还积压多少」写进状态表。
//
// 文档 15.1 只把「单轮上限被塞满」当成一句排查提示，实现里却既没有补偿路径也没有
// 告警：积压只能靠人去猜。这几个键把猜测变成可查状态，被截断时另外打一条 WARN。
//
// cursor 传的是本轮写完之后的位置，积压探针直接复用它，避免自己另算一套口径
// （两套口径迟早漂移，页面上的积压数字就会与采集器实际进度对不上）。
func (s *ReconciliationSyncService) recordCollectProgress(ctx context.Context, cursor reconciliationUsageCursor, batchSize int, truncated bool, now time.Time) {
	setStateValue(ctx, s.stateRepo, ReconciliationStateKeyUsageBatchSize, strconv.Itoa(batchSize))
	setStateValue(ctx, s.stateRepo, ReconciliationStateKeyUsageTruncated, strconv.FormatBool(truncated))

	if !truncated {
		// 没被截断说明窗口已经读完，积压清零；否则会留下一个骗人的旧数字。
		setStateValue(ctx, s.stateRepo, ReconciliationStateKeyUsageBacklog, "0")
		return
	}

	// 只有确实存在积压时才去数：这是唯一值得付出一次计数代价的场景。
	pending, err := s.usageSource.CountUsagePending(ctx, ReconciliationUsageQuery{
		From:       cursor.At.Add(-reconciliationCollectOverlap),
		To:         now,
		BoundaryAt: cursor.At,
		BoundaryID: cursor.ID,
		Limit:      reconciliationBacklogProbeLimit,
	}, reconciliationBacklogProbeLimit)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "usage_backlog_probe_failed: err=%v", err)
		return
	}
	setStateValue(ctx, s.stateRepo, ReconciliationStateKeyUsageBacklog, strconv.FormatInt(pending, 10))

	logger.LegacyPrintf("service.reconciliation_sync",
		"warn: usage_collect_truncated: batch_limit=%d pending_at_least=%d collected_at=%s — 单轮上限被塞满，游标只推进到本批最后一行，剩余行会在后续轮次继续采集",
		batchSize, pending, now.Format(time.RFC3339Nano))
}

// setStateValue 写一个状态键；写失败只记日志，不影响采集本身的结果。
func setStateValue(ctx context.Context, repo ReconciliationSyncStateRepository, key, value string) {
	if err := repo.Set(ctx, key, value); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "state_write_failed: key=%s err=%v", key, err)
	}
}

// CollectBacklog 返回当前待采集的行数，供只读诊断使用。
//
// 它不推进任何状态，也不写库：运维要先看见积压，才能决定要不要回拨游标重采。
// 返回值等于 reconciliationBacklogProbeLimit 时含义是「至少还有这么多」。
func (s *ReconciliationSyncService) CollectBacklog(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	cursor, err := s.resolveUsageCursor(ctx, now)
	if err != nil {
		return 0, err
	}
	return s.usageSource.CountUsagePending(ctx, ReconciliationUsageQuery{
		From:       cursor.At.Add(-reconciliationCollectOverlap),
		To:         now,
		BoundaryAt: cursor.At,
		BoundaryID: cursor.ID,
		Limit:      reconciliationBacklogProbeLimit,
	}, reconciliationBacklogProbeLimit)
}

// maxTime 返回两个时间中较晚的一个。
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
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

	bills, fetchErr := s.billSource.FetchBills(ctx, ReconciliationBillQuery{
		From:     from,
		To:       to,
		PageSize: s.cfg.StagingLimit,
	})

	// 拉取报错但已经拿到部分账单时：先把拿到的导入，再把错误报出去。
	//
	// 典型场景是翻页撞上限（ErrReconciliationA6PageLimitReached）：上游真实返回了
	// 好几页账单，旧实现却在导入之前就 return，把它们整批丢掉。下一轮又从第 1 页
	// 重新拉、再次撞上限、再次丢掉——导入进度永远是 0，而库里、页面上只有一条错误，
	// 看起来像「上游没数据」。
	//
	// 账单写入是幂等 upsert（provider + upstream_request_id 唯一，冲突 DO NOTHING），
	// 部分导入不会留下半截状态，所以「先导入、再报错」既安全又不静默：
	// 错误照旧记录并回传给调用方，只是不再连带丢掉已经花钱拉回来的数据。
	if fetchErr != nil {
		s.recordSyncError(ctx, fetchErr)
		if len(bills) == 0 {
			return 0, fetchErr
		}
		logger.LegacyPrintf(
			"service.reconciliation_sync",
			"a6_bills_partial_import: fetched=%d err=%v",
			len(bills), fetchErr,
		)
	}

	inserted, err := s.importBills(ctx, bills)
	if err != nil {
		s.recordSyncError(ctx, err)
		return inserted, err
	}

	if fetchErr != nil {
		// 部分导入：错误必须继续挂在页面上（不清 last_sync_error），
		// 也不算一次成功的同步（不刷新 last_sync_unix），否则运维会以为已经好了。
		return inserted, fetchErr
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

// importBills 把上游账单换算并幂等写入。
//
// 导入时把换算数字抄到每条账单上，落库即冻结：汇率后来变了也不能回头改历史账单。
//
// 同时从 raw 报文里解出上游令牌 ID（token_id）。它不落成数据库列——原始报文
// 已经整份存进 raw jsonb，这里只是让同一进程内的下游（例如手工导入后立刻触发的匹配）
// 拿到稳定标识，不必为了一个数字再解析一遍 JSON。
func (s *ReconciliationSyncService) importBills(ctx context.Context, bills []ReconciliationUpstreamBillPayload) (int64, error) {
	if len(bills) == 0 {
		return 0, nil
	}

	fxRate := s.EffectiveFxRate(ctx)
	for i := range bills {
		if bills[i].TokenID <= 0 {
			bills[i].TokenID = reconciliationTokenIDFromRaw(bills[i].Raw)
		}
		if bills[i].FxRateToCNY <= 0 {
			bills[i].FxRateToCNY = fxRate
		}
		bills[i].CostCNY = decimal.NewFromFloat(bills[i].CostOriginal).
			Mul(decimal.NewFromFloat(bills[i].FxRateToCNY)).
			InexactFloat64()
	}

	return s.billRepo.UpsertBatch(ctx, bills)
}

// reconciliationTokenIDFromRaw 从上游原始报文里取出令牌 ID。
//
// raw 的实际类型是 map[string]any，但里面数字的 Go 类型取决于解码方式：
// A6 客户端用 json.Decoder(UseNumber) 解出的是 json.Number，
// 手工重放（以及直接构造 map 的测试）传进来的则是 float64。
// 这里统一交给 a6Int64 收敛，不在本函数里穷举类型——为同一个字段写两份类型开关，
// 迟早漏掉一种形态，而漏掉的后果是「按 ID 匹配静默失效」。
//
// 返回 0 表示这条报文没有可用的稳定标识（字段缺失、为 null、或不是正整数）。
func reconciliationTokenIDFromRaw(raw map[string]any) int64 {
	if len(raw) == 0 {
		return 0
	}
	// 驼峰写法一并容纳：上游字段命名在 token_name/tokenName 上已经出现过两种写法，
	// 这里沿用同一套容忍策略，避免下次上游改名时又静默失配。
	value, ok := a6Lookup(raw, "token_id", "tokenId")
	if !ok {
		return 0
	}
	tokenID, ok := a6Int64(value)
	if !ok || tokenID <= 0 {
		return 0
	}
	return tokenID
}

// reconciliationPayloadTokenID 返回账单的令牌 ID：结构体上已解析好的直接用，
// 否则从 raw 兜底解析。
//
// 兜底是必要的：匹配阶段读的是**数据库里**的账单，而 token_id 没有独立列，
// 只能随 raw 一起取回来再解析（见 ReconciliationUpstreamBillRepository.ListStaging）。
func reconciliationPayloadTokenID(payload *ReconciliationUpstreamBillPayload) int64 {
	if payload == nil {
		return 0
	}
	if payload.TokenID > 0 {
		return payload.TokenID
	}
	return reconciliationTokenIDFromRaw(payload.Raw)
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

// reconciliationTokenKey 是匹配阶段「上游令牌身份」的缓存键。
//
// 必须同时带名字与 ID：改名之后，名字与 ID 的组合才是唯一身份；
// 只按名字缓存会把「名字相同、ID 不同」的两条账单算成同一个账号集合。
// tokenID 为 0 表示这条账单没有稳定标识，此键等价于旧行为（只按名字）。
type reconciliationTokenKey struct {
	name    string
	tokenID int64
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

	// 账号解析结果按「令牌名 + 令牌 ID」缓存，避免同一令牌反复查库。
	accountCache := make(map[reconciliationTokenKey][]int64, 8)
	now := time.Now().UTC()

	var matched int64
	var orphanIDs []int64

	for i := range bills {
		bill := &bills[i]

		// 令牌 ID 改名不变，优先从 raw 报文里取；取不到（历史手工导入）返回 0，
		// 此时退回纯名字匹配，与旧行为一致。
		tokenID := reconciliationPayloadTokenID(&bill.Payload)
		cacheKey := reconciliationTokenKey{name: strings.TrimSpace(bill.Payload.TokenName), tokenID: tokenID}

		accountIDs, ok := accountCache[cacheKey]
		if !ok {
			accountIDs, err = s.resolveAccountIDsForToken(ctx, cacheKey.name, cacheKey.tokenID)
			if err != nil {
				return matched, 0, err
			}
			accountCache[cacheKey] = accountIDs
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
		//
		// 宽限期必须从**账单导入本站的时刻**起算（文档 6.3「账单导入后有宽限期」），
		// 不能从 occurred_at 起算：账单是上游的历史流水，首次拉取 24 小时窗口时
		// 每一条 occurred_at 都已经超过 30 分钟，于是所有账单在第一轮就被判定成孤儿，
		// 宽限期形同不存在——线上表现是「刚导入就整批变孤儿」，而其中大部分其实
		// 只是还没轮到匹配（下游快照尚未采集齐）。
		if now.Sub(graceBase(bill)) >= s.cfg.MatchGracePeriod {
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

// RequeueUnmatched 把「当前规则确实能解析出账号」的孤儿账单退回匹配队列，返回退回条数。
//
// 存在的理由（生产实测的硬缺陷）：账单一旦被判成 unmatched，ListStaging 就再也不会捞它，
// 于是「管理员改对了令牌名」这个动作对历史账单完全无效：
// staging=37 / unmatched=3500 / matched=0，把 3 个填错的令牌名改对后本可匹配上 2487 条，
// 它们却全卡在 unmatched 里永远不复活。这个方法是那条唯一的复活通道，
// 由管理员显式点击触发，不做成常驻后台任务。
//
// 只退「能解析出账号」的那些，不做无差别退回：无差别退回会把一批注定匹配不上的账单
// 反复送回宽限期、每轮重扫、刷满日志，真正该重试的反而被淹没。
//
// 判定复用匹配阶段的 reconciliationRuleMatchesToken：规则里写 id:<数字> 的
// （改名不变的稳定标识）同样能通过守卫。这一点不能下推到 SQL 里用
// `external_key = token_name` 之类的条件代替——那种写法既认不出逗号分隔的多个标识，
// 也看不见只存在于 raw 报文里的 token_id，会让 Task A 的 ID 匹配在最需要它的场景下失效。
func (s *ReconciliationSyncService) RequeueUnmatched(ctx context.Context, from, to time.Time) (int64, error) {
	bills, err := s.billRepo.ListUnmatched(ctx, from, to, s.requeueUnmatchedLimit())
	if err != nil {
		return 0, err
	}
	if len(bills) == 0 {
		return 0, nil
	}

	rules, err := s.ruleRepo.List(ctx)
	if err != nil {
		return 0, err
	}

	ids := make([]int64, 0, len(bills))
	for i := range bills {
		payload := &bills[i].Payload
		if !reconciliationRulesMatchToken(rules, payload.TokenName, reconciliationTokenIDFromRaw(payload.Raw)) {
			continue
		}
		ids = append(ids, bills[i].ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	requeued, err := s.billRepo.RequeueUnmatched(ctx, ids)
	if err != nil {
		return requeued, err
	}
	logger.LegacyPrintf("service.reconciliation_sync",
		"requeue_unmatched: candidates=%d eligible=%d requeued=%d from=%s to=%s",
		len(bills), len(ids), requeued, from.Format(time.RFC3339), to.Format(time.RFC3339))
	return requeued, nil
}

// requeueUnmatchedLimit 是单次退回的条数上限。
//
// 与单轮匹配上限（StagingLimit）保持一致：退回的这批必须能被随后的一次匹配完整覆盖，
// 否则多退出来的账单会滞留在 staging——既不在「上游待匹配」计数里，也没被匹配上，
// 这种「看起来消失了」的状态比不退更糟。剩下的一批下次再点即可（动作是幂等的）。
func (s *ReconciliationSyncService) requeueUnmatchedLimit() int {
	if s.cfg.StagingLimit > 0 {
		return s.cfg.StagingLimit
	}
	return 1000
}

// reconciliationRulesMatchToken 判断启用的规则里是否至少有一条覆盖这条账单的令牌。
//
// 名字与 ID 都没有的账单直接判定为不可重试：退回去也只会再孤儿一次。
func reconciliationRulesMatchToken(rules []ReconciliationAccountRule, tokenName string, tokenID int64) bool {
	tokenName = strings.TrimSpace(tokenName)
	if tokenName == "" && tokenID <= 0 {
		return false
	}
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if reconciliationRuleMatchesToken(rule.ExternalKey, tokenName, tokenID) {
			return true
		}
	}
	return false
}

// graceBase 返回孤儿宽限期的起算时刻。
//
// 优先用账单入库时间（imported_at）：它才是文档 6.3 说的「账单导入后」。
// 只有在实现方没能给出入库时间时才退回 occurred_at——那是旧口径，会让首次
// 拉取的历史账单立刻过期；保留兜底只是为了不让缺字段的实现直接失去孤儿判定能力。
func graceBase(bill *ReconciliationUpstreamBill) time.Time {
	if bill == nil {
		return time.Time{}
	}
	if !bill.ImportedAt.IsZero() {
		return bill.ImportedAt
	}
	return bill.Payload.OccurredAt
}

// resolveAccountIDsForToken 找出某条上游账单可能对应的本站账号。
//
// 令牌的身份由**名字 + ID** 共同表达，两者命中任意一个都算候选，查三处：
//  1. 当前规则表 —— 规则的令牌标识集合里含有这个令牌名，或含有这个 token_id；
//  2. 历史快照表 —— 调用发生时该账号用的是这个名字（按账单上的名字反查）。
//
// 第 2 处是修复「令牌改名后历史账单变孤儿」的关键：旧实现只查第 1 处，
// 管理员把令牌从 0.12 改名成 0.15-claude 之后，所有旧账单立刻无法回配，
// 线上实测影响 1157 条。
//
// 第 1 处的 ID 通道是修复「上游改令牌名」的关键：历史账单里冻结的是 glm-3.5-95%，
// A6 后台现在叫 glm，两边名字对不上；但账单报文里的 token_id 与当前令牌 ID 相同，
// 于是规则写成 "glm, id:41210" 就能同时覆盖新名与历史名，改名不再造成失配。
func (s *ReconciliationSyncService) resolveAccountIDsForToken(ctx context.Context, tokenName string, tokenID int64) ([]int64, error) {
	tokenName = strings.TrimSpace(tokenName)
	// 名字与 ID 都没有时无事可做：直接返回空候选集，账单会留在 staging 等宽限期。
	if tokenName == "" && tokenID <= 0 {
		return nil, nil
	}

	rules, err := s.ruleRepo.List(ctx)
	if err != nil {
		return nil, err
	}

	seen := make(map[int64]struct{}, 4)
	ids := make([]int64, 0, 4)
	for _, rule := range rules {
		if !rule.Enabled || !reconciliationRuleMatchesToken(rule.ExternalKey, tokenName, tokenID) {
			continue
		}
		if _, exists := seen[rule.AccountID]; exists {
			continue
		}
		seen[rule.AccountID] = struct{}{}
		ids = append(ids, rule.AccountID)
	}

	// 历史快照只按名字反查：快照冻结的是「调用发生时该账号配的令牌标识」，
	// 用账单上的名字去比对，才能覆盖「规则后来改成了别的写法」的情况。
	// 名字为空（只配了 id: 的规则）时没有可反查的名字，跳过即可——
	// 那种情况下当前规则表的 ID 通道已经给出了账号集合。
	if tokenName != "" {
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
	}

	return ids, nil
}

// reconciliationRuleMatchesToken 判断一条规则的令牌标识集合是否覆盖账单上的令牌。
//
// 解析一律走 ParseReconciliationExternalKeys，不在匹配侧另写一套逗号/前缀判断：
// 校验、展示、匹配三处共用同一个解析实现，才不会出现「保存时允许、匹配时不认」这种
// 最难排查的口径漂移。
func reconciliationRuleMatchesToken(externalKey, tokenName string, tokenID int64) bool {
	names, tokenIDs := ParseReconciliationExternalKeys(externalKey)

	if tokenID > 0 {
		for _, candidate := range tokenIDs {
			if candidate == tokenID {
				return true
			}
		}
	}
	if tokenName != "" {
		for _, candidate := range names {
			if candidate == tokenName {
				return true
			}
		}
	}
	return false
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
		if err := s.RunOnce(runCtx); err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "trigger_async_run_failed: err=%v", err)
		}
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

	// 采集进度可观测字段。它们是 P0 事故的「事前告警面」：
	// 旧实现在被单轮上限截断时悄无声息，积压只能靠人去猜。
	//
	// UsageCursorAt 是采集游标；UsageLastBatchSize 是本轮取到的行数；
	// UsageBatchTruncated 为 true 表示本轮被上限塞满（仍有行待采集）；
	// UsageBacklog 是待采集行数（等于 reconciliationBacklogProbeLimit 时表示「至少这么多」）。
	UsageCursorAt       *time.Time
	UsageLastBatchSize  int64
	UsageBatchTruncated bool
	UsageBacklog        int64
}

// Status 读取当前同步状态。
func (s *ReconciliationSyncService) Status(ctx context.Context) (*ReconciliationSyncStatus, error) {
	keys := []string{
		ReconciliationStateKeyA6LastSyncUnix,
		ReconciliationStateKeyA6LastSyncError,
		ReconciliationStateKeyA6LastSyncErrorAt,
		ReconciliationStateKeyUsageCursor,
		ReconciliationStateKeyUsageBatchSize,
		ReconciliationStateKeyUsageTruncated,
		ReconciliationStateKeyUsageBacklog,
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
	if raw := strings.TrimSpace(values[ReconciliationStateKeyUsageCursor]); raw != "" {
		if at, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
			status.UsageCursorAt = &at
		}
	}
	if raw := strings.TrimSpace(values[ReconciliationStateKeyUsageBatchSize]); raw != "" {
		if size, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && size >= 0 {
			status.UsageLastBatchSize = size
		}
	}
	status.UsageBatchTruncated = strings.EqualFold(strings.TrimSpace(values[ReconciliationStateKeyUsageTruncated]), "true")
	if raw := strings.TrimSpace(values[ReconciliationStateKeyUsageBacklog]); raw != "" {
		if pending, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && pending >= 0 {
			status.UsageBacklog = pending
		}
	}
	// 采集中枢依赖的状态都可读时就算健康；上游凭据缺失单独由 BillSourceReady 表达，
	// 不让整个页面显示为故障——本地对账数据依然可用。
	//
	// 采集被上限截断**不**算不健康：那是采集器在正常工作、只是没追上写入速度，
	// 会在后续轮次继续推进。把它算成故障会让页面长期挂着红点，反而没人看。
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
