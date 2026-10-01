package service

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

// 采集器的 leader 锁参数。多实例部署时同一时刻只允许一个实例真正去采集，
// 避免 N 个实例同时对上游发请求、同时写同一批账单。
const (
	reconciliationLeaderLockKey = "reconciliation:collector:leader"
	reconciliationLeaderLockTTL = 2 * time.Minute
)

// ReconciliationCollectorConfig 是采集器的调度参数。
type ReconciliationCollectorConfig struct {
	// Enabled 是否启动周期采集。
	Enabled bool
	// UsageInterval 下游用量采集的间隔。
	UsageInterval time.Duration
	// BillInterval 上游账单同步与匹配的间隔。
	BillInterval time.Duration

	// RealtimeEnabled 是否在发现新扣费记录后立刻拉上游账单并匹配。
	RealtimeEnabled bool
	// RealtimeDelay 发现新记录后延迟多久才动手。留一段时间让上游先把账单落库。
	RealtimeDelay time.Duration
	// RealtimeMinInterval 两次实时匹配之间的最小间隔，防止把上游打限流。
	RealtimeMinInterval time.Duration
	// RealtimeWindow 实时匹配回看的窗口长度。
	RealtimeWindow time.Duration
}

// ReconciliationCollector 周期性地采集下游用量、同步上游账单并完成匹配。
//
// 这是把对账从「手动点一下才动」变成「自己会动」的部分。两个循环刻意用不同的
// 节奏：用量采集要快（30 秒），因为本地数据本来就在自己库里、代价极低；
// 上游账单同步要慢（5 分钟），因为每次都要打上游接口，打太快没有意义还可能被限流。
type ReconciliationCollector struct {
	syncSvc *ReconciliationSyncService
	cfg     ReconciliationCollectorConfig

	lockCache LeaderLockCache
	db        *sql.DB

	parentCtx    context.Context
	parentCancel context.CancelFunc

	mu         sync.Mutex
	wg         sync.WaitGroup
	started    bool
	stopped    bool
	instanceID string

	// realtimeCh 是「刚刚有新的扣费记录」这一个事实的投递通道。
	//
	// 容量刻意只有 1：它传递的不是「有几条记录」而是「有新记录」这个状态，
	// 短时间内的上千条调用合并成一次拉取就够了。容量放大反而会让实时循环
	// 积压出一串过期的唤醒，每次都去拉一遍上游。
	realtimeCh chan struct{}
}

// NewReconciliationCollector 创建采集器。构造阶段不启动任何后台任务。
func NewReconciliationCollector(syncSvc *ReconciliationSyncService, cfg ReconciliationCollectorConfig) *ReconciliationCollector {
	if cfg.UsageInterval <= 0 {
		cfg.UsageInterval = 30 * time.Second
	}
	if cfg.BillInterval <= 0 {
		cfg.BillInterval = 5 * time.Minute
	}
	if cfg.RealtimeDelay <= 0 {
		cfg.RealtimeDelay = 30 * time.Second
	}
	if cfg.RealtimeMinInterval <= 0 {
		cfg.RealtimeMinInterval = time.Minute
	}
	if cfg.RealtimeWindow <= 0 {
		cfg.RealtimeWindow = 15 * time.Minute
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	return &ReconciliationCollector{
		syncSvc:      syncSvc,
		cfg:          cfg,
		parentCtx:    parentCtx,
		parentCancel: parentCancel,
		instanceID:   uuid.NewString(),
		realtimeCh:   make(chan struct{}, 1),
	}
}

// SetLeaderLock 注入 leader 锁所需的两级依赖：优先 Redis，退化到数据库咨询锁。
func (c *ReconciliationCollector) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if c == nil {
		return
	}
	c.lockCache = lockCache
	c.db = db
}

// Start 启动后台循环。重复调用无副作用。
func (c *ReconciliationCollector) Start() {
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
	if c.cfg.RealtimeEnabled {
		c.wg.Add(1)
		go c.runRealtimeLoop()
	}
	c.mu.Unlock()
	go c.runLoop()
}

// NotifyUsageRecorded 告知采集器「刚刚产生了新的下游扣费记录」。
//
// 这是把对账从「定时批量」变成「跟着流量走」的入口。它有两个硬约束：
//
//  1. 绝不阻塞调用方。投递失败（通道已满）就直接丢弃：丢一次通知只是晚一点
//     对账，而阻塞一次请求是事故。调用方拿不到返回值，也就不可能误以为
//     「通知成功」等于「账单已匹配」。
//  2. 必须可合并。一条通知代表的是「有新记录」这个状态而不是「一条记录」，
//     因此容量为 1 的通道天然把密集流量合并成一次拉取。
//
// 采集器未启用实时匹配时该方法完全空转，调用方不需要判空、不需要分支。
func (c *ReconciliationCollector) NotifyUsageRecorded() {
	if c == nil || !c.cfg.RealtimeEnabled {
		return
	}
	select {
	case c.realtimeCh <- struct{}{}:
	default:
	}
}

// Stop 停止后台循环并等待在途任务收尾。
func (c *ReconciliationCollector) Stop() {
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

// runLoop 是两个周期任务的调度骨架。
func (c *ReconciliationCollector) runLoop() {
	defer c.wg.Done()

	usageTicker := time.NewTicker(c.cfg.UsageInterval)
	defer usageTicker.Stop()
	billTicker := time.NewTicker(c.cfg.BillInterval)
	defer billTicker.Stop()

	// 启动时把实时匹配的三要素一起打出来。
	//
	// 这不是为了好看：实时匹配是「有流量才动」的，一旦参数配错（比如窗口为 0
	// 或被环境变量意外关掉），现象是「什么都没发生」——没有报错、没有日志、
	// 匹配数就是不涨。把开关与三个参数在启动时留痕，是唯一能让运维一眼分辨
	// 「没开」与「开了但没触发」的手段。
	logger.LegacyPrintf("service.reconciliation_collector",
		"collector_started: usage_interval=%s bill_interval=%s realtime_enabled=%v realtime_delay=%s realtime_min_interval=%s realtime_window=%s",
		c.cfg.UsageInterval, c.cfg.BillInterval,
		c.cfg.RealtimeEnabled, c.cfg.RealtimeDelay, c.cfg.RealtimeMinInterval, c.cfg.RealtimeWindow)

	for {
		select {
		case <-c.parentCtx.Done():
			logger.LegacyPrintf("service.reconciliation_collector", "collector_stopped: instance=%s", c.instanceID)
			return
		case <-usageTicker.C:
			c.runWithLeaderLock(c.collectUsageOnce, "usage")
		case <-billTicker.C:
			c.runWithLeaderLock(c.syncBillsOnce, "bills")
		}
	}
}

// runWithLeaderLock 在抢到 leader 锁时执行一次任务；抢不到说明别的实例在做，本轮跳过。
func (c *ReconciliationCollector) runWithLeaderLock(task func(context.Context), label string) {
	if c.parentCtx.Err() != nil {
		return
	}

	release, acquired, err := c.tryAcquireLeaderLock(c.parentCtx, reconciliationLeaderLockKey)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_collector", "leader_lock_error: task=%s err=%v", label, err)
		return
	}
	if !acquired {
		return
	}
	defer release()

	task(c.parentCtx)
}

// tryAcquireLeaderLock 依次尝试 Redis 锁与数据库咨询锁，都没有时退化为单实例假设。
func (c *ReconciliationCollector) tryAcquireLeaderLock(ctx context.Context, key string) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if c.lockCache != nil {
		acquired, err := c.lockCache.TryAcquireLeaderLock(lockCtx, key, c.instanceID, reconciliationLeaderLockTTL)
		if err != nil {
			return nil, false, err
		}
		if !acquired {
			return nil, false, nil
		}
		return func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = c.lockCache.ReleaseLeaderLock(releaseCtx, key, c.instanceID)
		}, true, nil
	}

	if c.db != nil {
		return tryAcquireDBAdvisoryLockWithError(lockCtx, c.db, hashAdvisoryLockID(key))
	}

	// 既没有 Redis 也没有数据库连接时按单实例处理，总比完全不动要好。
	return func() {}, true, nil
}

// collectUsageOnce 采集一轮下游用量。
func (c *ReconciliationCollector) collectUsageOnce(ctx context.Context) {
	inserted, err := c.syncSvc.CollectUsage(ctx)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_collector", "collect_usage_failed: err=%v", err)
		return
	}
	if inserted > 0 {
		logger.LegacyPrintf("service.reconciliation_collector", "collect_usage_ok: inserted=%d", inserted)
		// 刚采到新的扣费记录，安排一次实时匹配。
		//
		// 这里刻意不去改请求链路：用量采集本身就是「新记录出现」的发现点，
		// 在它这里触发既拿得到准确时机，又完全不必碰代理与计费代码。
		c.NotifyUsageRecorded()
	}
}

// runRealtimeLoop 是实时匹配的调度循环。
//
// 它只做一件事：收到「有新扣费记录」的信号后，等 RealtimeDelay 让上游把账单
// 结算完，再拉一小段账单并匹配一次；两次执行之间强制间隔 RealtimeMinInterval。
//
// 两个设计取舍值得写下来：
//
//  1. 为什么要延迟而不是立刻去拉。上游账单通常晚于本站调用落库，发现调用就
//     立刻拉多半一条也拉不到，白白花掉一次上游配额；等几十秒再拉一次，
//     命中率完全不同。
//  2. 为什么延迟期间到达的新信号不重置计时器。若每条新调用都把计时器往后推，
//     持续流量下计时器永远不会到点——实时匹配看起来在工作，实际一次都没跑。
//     宁可晚一点执行，也不能出现这种「假工作」。
func (c *ReconciliationCollector) runRealtimeLoop() {
	defer c.wg.Done()

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	var lastRun time.Time
	armed := false

	for {
		select {
		case <-c.parentCtx.Done():
			return
		case <-c.realtimeCh:
			if armed {
				// 已经排过一次，等它到点即可——这就是「合并密集通知」。
				continue
			}
			timer.Reset(c.cfg.RealtimeDelay)
			armed = true
		case <-timer.C:
			armed = false
			if !lastRun.IsZero() {
				if wait := c.cfg.RealtimeMinInterval - time.Since(lastRun); wait > 0 {
					timer.Reset(wait)
					armed = true
					continue
				}
			}
			c.runWithLeaderLock(c.realtimeMatchOnce, "realtime")
			lastRun = time.Now()
		}
	}
}

// realtimeMatchOnce 拉一小段上游账单并立刻匹配。
//
// 与 syncBillsOnce 的唯一区别是窗口：这里只回看 RealtimeWindow（默认 15 分钟），
// 因为触发它的是刚刚发生的调用。两条线互不替代——实时线负责「快」，定时线负责「全」，
// 实时线漏掉的（进程重启、上游迟到、被限流）由定时线兜底。
func (c *ReconciliationCollector) realtimeMatchOnce(ctx context.Context) {
	now := time.Now().UTC()
	from := now.Add(-c.cfg.RealtimeWindow)

	imported, err := c.syncSvc.SyncA6Bills(ctx, from, now)
	switch {
	case err == nil:
		if imported > 0 {
			logger.LegacyPrintf("service.reconciliation_collector", "realtime_bills_ok: imported=%d window=%s", imported, c.cfg.RealtimeWindow)
		}
	case errors.Is(err, ErrReconciliationBillSourceUnavailable):
		// 上游凭据还没配好。这不是故障，只是功能未启用，不打错误日志。
		return
	default:
		logger.LegacyPrintf("service.reconciliation_collector", "realtime_bills_failed: err=%v", err)
		return
	}

	matched, unmatched, err := c.syncSvc.MatchStaging(ctx, from, now)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_collector", "realtime_match_failed: err=%v", err)
		return
	}
	// 只在真的配上时才打日志：实时线每有新调用就跑，matched=0 是常态，
	// 每次都打一行会把日志刷成噪声。
	if matched > 0 {
		logger.LegacyPrintf("service.reconciliation_collector", "realtime_match_ok: matched=%d unmatched=%d", matched, unmatched)
	}
}

// syncBillsOnce 同步一轮上游账单并完成匹配。
func (c *ReconciliationCollector) syncBillsOnce(ctx context.Context) {
	now := time.Now().UTC()
	lookback := c.syncSvc.cfg.A6Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	from := now.Add(-lookback)

	imported, err := c.syncSvc.SyncA6Bills(ctx, from, now)
	switch {
	case err == nil:
		if imported > 0 {
			logger.LegacyPrintf("service.reconciliation_collector", "sync_bills_ok: imported=%d", imported)
		}
	case errors.Is(err, ErrReconciliationBillSourceUnavailable):
		// 上游凭据还没配好。这不是故障，只是功能未启用，不打错误日志刷屏。
		return
	default:
		logger.LegacyPrintf("service.reconciliation_collector", "sync_bills_failed: err=%v", err)
	}

	matched, unmatched, err := c.syncSvc.MatchStaging(ctx, from, now)
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_collector", "match_staging_failed: err=%v", err)
		return
	}
	if matched > 0 || unmatched > 0 {
		logger.LegacyPrintf("service.reconciliation_collector", "match_staging_ok: matched=%d unmatched=%d", matched, unmatched)
	}
}

// ProvideReconciliationCollector 构造并启动进程级的对账采集器。
func ProvideReconciliationCollector(
	syncSvc *ReconciliationSyncService,
	lockCache LeaderLockCache,
	db *sql.DB,
	cfg *config.Config,
) *ReconciliationCollector {
	recon := cfg.Reconciliation
	collector := NewReconciliationCollector(syncSvc, ReconciliationCollectorConfig{
		Enabled:       recon.Enabled,
		UsageInterval: time.Duration(recon.UsageIntervalSeconds) * time.Second,
		BillInterval:  time.Duration(recon.A6SyncIntervalSeconds) * time.Second,

		RealtimeEnabled:     recon.RealtimeMatchEnabled,
		RealtimeDelay:       time.Duration(recon.RealtimeMatchDelaySeconds) * time.Second,
		RealtimeMinInterval: time.Duration(recon.RealtimeMatchMinIntervalSeconds) * time.Second,
		RealtimeWindow:      time.Duration(recon.RealtimeMatchWindowSeconds) * time.Second,
	})
	collector.SetLeaderLock(lockCache, db)
	collector.Start()
	return collector
}
