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
}

// NewReconciliationCollector 创建采集器。构造阶段不启动任何后台任务。
func NewReconciliationCollector(syncSvc *ReconciliationSyncService, cfg ReconciliationCollectorConfig) *ReconciliationCollector {
	if cfg.UsageInterval <= 0 {
		cfg.UsageInterval = 30 * time.Second
	}
	if cfg.BillInterval <= 0 {
		cfg.BillInterval = 5 * time.Minute
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	return &ReconciliationCollector{
		syncSvc:      syncSvc,
		cfg:          cfg,
		parentCtx:    parentCtx,
		parentCancel: parentCancel,
		instanceID:   uuid.NewString(),
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
	c.mu.Unlock()
	go c.runLoop()
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

	logger.LegacyPrintf("service.reconciliation_collector", "collector_started: usage_interval=%s bill_interval=%s",
		c.cfg.UsageInterval, c.cfg.BillInterval)

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
	})
	collector.SetLeaderLock(lockCache, db)
	collector.Start()
	return collector
}
