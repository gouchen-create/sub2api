package service

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// 上游成本取数（把「这笔调用在上游 A6 真实被扣了多少钱」写回使用记录）。
//
// 为什么是「按请求 ID 反查」而不是「按时间窗口批量拉账单」：
// 该 A6 账号与其它系统共用，批量拉取会把别人的扣费一并拉进本库，于是不得不
// 额外维护「这不是我们的账单」这类状态、宽限期、人工退回等等——这些功能全都
// 在为别人的消费服务，属于纯粹的多余复杂度。按 ID 反查天然只拿本系统自己那条。
//
// 关键时间常量来自线上实测（2026-10-02，200 条真实账单的「请求发生 → 账单可查」
// 间隔）：P50≈13s、P90≈66s、P95≈111s、P99≈599s、最大 600s。
// 也就是说账单**不是**即时落库的，「调用一结束就查」有一半以上会查空。
// 因此流程必然是：先等一段时间，再查，查不到就退避重试，重试用尽才认输。

const upstreamCostLogComponent = "service.upstream_cost"

// 取数状态**不落库**，全部由 upstream_request_id / upstream_cost_fetched_at /
// upstream_cost_attempts 三个字段推导出来（理由见 246 号迁移的注释，核心是不必为了
// 一个可推导的字段去改全站最热的 9 条 INSERT 语句，也不必回填 49 万条历史行）：
//
//	待查   upstream_request_id 非空 且 fetched_at 为空 且 attempts < MaxAttempts
//	已取到 fetched_at 非空
//	未取到 fetched_at 为空 且 attempts >= MaxAttempts（重试用尽，认输）
//	不适用 upstream_request_id 为空（本功能上线前的全部存量记录）

// 取数任务的 leader 锁。多实例部署时只允许一个实例真的去打上游接口。
const (
	upstreamCostLeaderLockKey = "upstream_cost:collector:leader"
	upstreamCostLeaderLockTTL = 2 * time.Minute
)

// UpstreamCostValue 是一条使用记录对应的上游真实扣费。
//
// 刻意**不含**任何换算结果：本站的「费用」是美元原值，对账要看的是「收了多少、
// 上游实际扣了多少」的差额，两边同币种直接相减才有意义。若在这里换成人民币，
// 页面上就变成一列美元一列人民币，得先心算汇率才能比较——换算在此不是增值而是添乱。
type UpstreamCostValue struct {
	// Original 上游账单里的原币金额。
	Original decimal.Decimal
	// Currency 原币币种，A6 为 USD。仅用于显示货币符号，不参与计算。
	Currency string
}

// UpstreamCostPending 是一条「该去查成本」的使用记录。
type UpstreamCostPending struct {
	// UsageLogID 使用记录主键。
	UsageLogID int64
	// RequestID 上游请求标识，取自 usage_logs.upstream_request_id。
	RequestID string
	// Attempts 已经查过几次。
	Attempts int16
	// CreatedAt 这笔记账的时刻，用于退避判定与日志。
	CreatedAt time.Time
}

// UpstreamCostRepository 是取数任务需要的持久化能力。
//
// 刻意只暴露三个动作，对应状态机的三条边；不提供「按任意条件查询」的口子，
// 避免取数任务以后长出报表职责。状态本身不落库，所以也没有「改状态」的动作。
type UpstreamCostRepository interface {
	// ListUpstreamCostPending 取一批待查记录，按记账时刻升序（先来先查，避免饿死）。
	//
	// 只返回同时满足以下条件的记录：
	//   - 带着非空的上游请求 ID（本功能上线前的历史行天然没有 ID，自动落在队列之外）；
	//   - 尚未取到成本（upstream_cost_fetched_at 为空）；
	//   - 记账时刻早于 now-firstDelay（账单落库需要时间，太早查是白跑）；
	//   - 已尝试次数小于 maxAttempts（用尽后不再占用队列）。
	ListUpstreamCostPending(ctx context.Context, now time.Time, firstDelay time.Duration, maxAttempts int, limit int) ([]UpstreamCostPending, error)
	// ResolveUpstreamCost 记录取到的成本（同时把 fetched_at 写上有值，即「已取到」）。
	ResolveUpstreamCost(ctx context.Context, usageLogID int64, value UpstreamCostValue, at time.Time) error
	// RetryUpstreamCost 记一次「查了但没有」：尝试次数加一。
	// 次数达到上限后，该记录自然不再进入待查队列，在报表上呈现为「未取到」。
	RetryUpstreamCost(ctx context.Context, usageLogID int64) error
}

// UpstreamCostCollectorConfig 是取数任务的调度参数。
type UpstreamCostCollectorConfig struct {
	// Enabled 是否启动周期取数。
	Enabled bool
	// Interval 扫描间隔。
	Interval time.Duration
	// FirstDelay 记账之后多久才第一次去查。
	//
	// 默认 90 秒：实测 P90≈66s，等 90 秒能让约九成账单一次就查到，
	// 既不用查空太多次，也不会让成本在页面上久等。
	FirstDelay time.Duration
	// MaxAttempts 最多查几次。
	//
	// 与 Interval 一起决定放弃的时刻：90s + 20×30s ≈ 11.5 分钟，
	// 覆盖实测最大落库延迟（600s）并留出余量。
	MaxAttempts int
	// BatchSize 每轮最多处理多少条。
	//
	// 必须有上限：突发流量会让待查记录瞬间堆到成千上万条，
	// 若不封顶就会在同一个 tick 里对上游连发上千次请求。超出的留到下一轮，
	// 按记账时刻升序处理，早的记录优先。
	BatchSize int
}

// Normalize 补齐缺省值，保证构造出来的调度参数一定可用。
func (cfg UpstreamCostCollectorConfig) Normalize() UpstreamCostCollectorConfig {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.FirstDelay <= 0 {
		cfg.FirstDelay = 90 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 20
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	return cfg
}

// UpstreamCostService 负责「查一批待取记录 → 逐条按 ID 反查 → 写回成本」。
type UpstreamCostService struct {
	repo     UpstreamCostRepository
	client   *A6Client
	settings *ReconciliationA6SettingsService
	cfg      UpstreamCostCollectorConfig
}

// NewUpstreamCostService 创建取数服务。构造阶段不做任何 IO。
func NewUpstreamCostService(
	repo UpstreamCostRepository,
	client *A6Client,
	settings *ReconciliationA6SettingsService,
	cfg UpstreamCostCollectorConfig,
) *UpstreamCostService {
	return &UpstreamCostService{repo: repo, client: client, settings: settings, cfg: cfg.Normalize()}
}

// UpstreamCostResult 是一轮取数的结果，用于日志与测试断言。
type UpstreamCostResult struct {
	// Examined 本轮从库里取出的待查记录数。
	Examined int
	// Resolved 成功取到成本的条数。
	Resolved int
	// Retried 查了但没有、留待下轮的条数。
	Retried int
	// Exhausted 本轮用尽重试次数、标记为未取到的条数。
	Exhausted int
	// Failed 查询本身失败（网络/鉴权/上游异常）的条数。
	Failed int
}

// RunOnce 执行一轮取数。返回的错误只表示「这一轮整体无法进行」，
// 单条记录的失败已经计入 UpstreamCostResult，不会中断整轮。
func (s *UpstreamCostService) RunOnce(ctx context.Context, now time.Time) (UpstreamCostResult, error) {
	var result UpstreamCostResult
	if s == nil || s.repo == nil {
		return result, nil
	}

	if s.client == nil {
		return result, ErrReconciliationA6NotConfigured
	}
	// 每轮都从设置里刷一次凭据：管理员在面板上改完，下一轮就该用上。
	if s.settings != nil {
		s.client.SetConfig(s.settings.Effective(ctx).ClientConfig())
	}
	if !s.client.Configured() {
		return result, ErrReconciliationA6NotConfigured
	}

	pending, err := s.repo.ListUpstreamCostPending(ctx, now, s.cfg.FirstDelay, s.cfg.MaxAttempts, s.cfg.BatchSize)
	if err != nil {
		return result, err
	}
	result.Examined = len(pending)

	for i := range pending {
		item := &pending[i]
		if err := ctx.Err(); err != nil {
			return result, err
		}
		s.resolveOne(ctx, item, &result)
	}
	return result, nil
}

// resolveOne 处理单条记录，把结果累加进 result。
func (s *UpstreamCostService) resolveOne(ctx context.Context, item *UpstreamCostPending, result *UpstreamCostResult) {
	bill, found, err := s.client.FetchBillByRequestID(ctx, item.RequestID)
	if err != nil {
		// 查询失败与「上游确实没有这条」是两回事，日志必须能区分开——
		// 否则「参数被忽略」「凭据失效」这类系统性问题会伪装成一条条查不到。
		result.Failed++
		s.bumpAttempts(ctx, item)
		logger.LegacyPrintf(upstreamCostLogComponent,
			"upstream_cost_query_failed: usage_log_id=%d attempts=%d err=%v",
			item.UsageLogID, item.Attempts+1, err)
		return
	}
	if !found {
		exhausted := int(item.Attempts)+1 >= s.cfg.MaxAttempts
		result.Retried++
		if exhausted {
			result.Exhausted++
		}
		s.bumpAttempts(ctx, item)
		return
	}

	value := UpstreamCostValue{
		Original: bill.CostUSD,
		Currency: "USD",
	}
	if err := s.repo.ResolveUpstreamCost(ctx, item.UsageLogID, value, time.Now().UTC()); err != nil {
		result.Failed++
		logger.LegacyPrintf(upstreamCostLogComponent,
			"upstream_cost_write_failed: usage_log_id=%d err=%v", item.UsageLogID, err)
		return
	}
	result.Resolved++
}

// bumpAttempts 记一次「查了但没有」；写库失败只记日志，不影响同轮其它记录。
func (s *UpstreamCostService) bumpAttempts(ctx context.Context, item *UpstreamCostPending) {
	if err := s.repo.RetryUpstreamCost(ctx, item.UsageLogID); err != nil {
		logger.LegacyPrintf(upstreamCostLogComponent,
			"upstream_cost_retry_mark_failed: usage_log_id=%d err=%v", item.UsageLogID, err)
	}
}

// ==================== 后台循环 ====================

// UpstreamCostCollector 周期性地把上游真实成本写回使用记录。
//
// 与旧的对账采集器相比刻意做得更薄：没有用量采集、没有账单批量同步、没有匹配、
// 没有孤儿状态。它只做一件事——把「已经产生但还没取到成本」的记录补齐。
type UpstreamCostCollector struct {
	svc *UpstreamCostService
	cfg UpstreamCostCollectorConfig

	lockCache LeaderLockCache
	db        *sql.DB

	parentCtx    context.Context
	parentCancel context.CancelFunc

	mu         sync.Mutex
	wg         sync.WaitGroup
	started    bool
	stopped    bool
	instanceID string

	// reportedNotConfigured 记录「上一轮是否因为上游凭据没配而暂停」，
	// 用于把这一持续状态压成一次日志（见 tick 里的说明）。
	reportedNotConfigured bool
}

// NewUpstreamCostCollector 创建取数采集器。构造阶段不启动任何后台任务。
//
// 刻意不做「刚记完账就立刻扫一轮」的唤醒通道：账单落库本身要等（实测 P50≈13s），
// 首次查询被 FirstDelay 挡在 90 秒之后，而扫描节奏是 30 秒一次——也就是说
// 任何唤醒都会落在「就算扫了也还不能查」的时间窗里，一点用都没有。
// 少一条通道就少一处调用方耦合，也少一类「通知丢了怎么办」的问题。
func NewUpstreamCostCollector(svc *UpstreamCostService, cfg UpstreamCostCollectorConfig) *UpstreamCostCollector {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	return &UpstreamCostCollector{
		svc:          svc,
		cfg:          cfg.Normalize(),
		parentCtx:    parentCtx,
		parentCancel: parentCancel,
		instanceID:   uuid.NewString(),
	}
}

// SetLeaderLock 注入 leader 锁所需的两级依赖：优先 Redis，退化到数据库咨询锁。
func (c *UpstreamCostCollector) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if c == nil {
		return
	}
	c.lockCache = lockCache
	c.db = db
}

// Start 启动后台循环。重复调用无副作用。
func (c *UpstreamCostCollector) Start() {
	if c == nil || !c.cfg.Enabled {
		return
	}
	c.mu.Lock()
	if c.started || c.stopped {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.wg.Add(1)
	c.mu.Unlock()
	go c.runLoop()
}

// Stop 停止后台循环并等待在途任务收尾。
func (c *UpstreamCostCollector) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	cancel := c.parentCancel
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// runLoop 是取数的调度骨架。
func (c *UpstreamCostCollector) runLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	logger.LegacyPrintf(upstreamCostLogComponent,
		"upstream_cost_collector_started: instance=%s interval=%s first_delay=%s max_attempts=%d batch_size=%d",
		c.instanceID, c.cfg.Interval, c.cfg.FirstDelay, c.cfg.MaxAttempts, c.cfg.BatchSize)

	// 启动先跑一轮：进程重启后可能有积压，不必等第一个 tick。
	c.tick()

	for {
		select {
		case <-c.parentCtx.Done():
			logger.LegacyPrintf(upstreamCostLogComponent,
				"upstream_cost_collector_stopped: instance=%s", c.instanceID)
			return
		case <-ticker.C:
			c.tick()
		}
	}
}

// tick 执行一轮，并负责 leader 判定与错误记录。
func (c *UpstreamCostCollector) tick() {
	if c.parentCtx.Err() != nil {
		return
	}
	// 多实例部署时保证同一时刻只有一个实例去查上游；拿不到锁说明别的实例正在做，
	// 本轮直接跳过即可，不算错误。未注入锁依赖时（单机/测试）恒为可执行。
	release, ok := tryAcquireSingletonLeaderLock(
		c.parentCtx, c.lockCache, c.db, upstreamCostLeaderLockKey, c.instanceID, upstreamCostLeaderLockTTL)
	if !ok {
		return
	}
	defer release()

	start := time.Now()
	result, err := c.svc.RunOnce(c.parentCtx, start)
	if err != nil {
		if c.parentCtx.Err() != nil {
			return
		}
		// 「上游凭据没配」是一个**持续状态**，不是一次事件：这个循环每 30 秒一轮，
		// 每轮都打一条错误日志会在管理员还没配好之前把日志刷满，把真正的故障淹掉。
		// 因此只在状态**翻转**时记录：第一次发现未配置记一条，配好之后恢复记一条。
		if errors.Is(err, ErrReconciliationA6NotConfigured) {
			if !c.reportedNotConfigured {
				c.reportedNotConfigured = true
				logger.LegacyPrintf(upstreamCostLogComponent,
					"upstream_cost_a6_unconfigured: 上游 A6 凭据未配置，取数暂停；配好后会自动继续（本状态只在翻转时记录一次）")
			}
			return
		}
		logger.LegacyPrintf(upstreamCostLogComponent, "upstream_cost_round_failed: err=%v", err)
		return
	}
	if c.reportedNotConfigured {
		c.reportedNotConfigured = false
		logger.LegacyPrintf(upstreamCostLogComponent,
			"upstream_cost_a6_recovered: 上游 A6 凭据已可用，取数恢复")
	}
	// 只在真的动了东西时打日志：这个循环每 30 秒一次，无脑打会把日志淹掉。
	//
	// ⚠️ 这个格式串里的字段名是**不能随便改**的：logger.LegacyPrintf 会按消息文本
	// 猜级别——只要出现 " failed"、"error"、"panic"、"fatal" 就判为 ERROR，
	// 出现 "warn"、"fallback" 就判为 WARN，其余才是 INFO。
	// 这里统计的是「本轮有几条查不通」，本来是个中性计数；如果把字段名写成
	// `failed=0`，一次完全成功的轮次也会因为字面上出现了 " failed" 而被记成 ERROR，
	// 把错误日志和告警全部带偏。因此刻意用 `query_faults` 这个不含触发词的名字。
	if result.Examined > 0 {
		logger.LegacyPrintf(upstreamCostLogComponent,
			"upstream_cost_round_done: examined=%d resolved=%d retried=%d exhausted=%d query_faults=%d elapsed=%s",
			result.Examined, result.Resolved, result.Retried, result.Exhausted, result.Failed,
			time.Since(start).Round(time.Millisecond))
	}
}
