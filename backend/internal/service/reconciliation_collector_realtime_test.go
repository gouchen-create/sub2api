//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 实时匹配：通知必须非阻塞，密集通知必须合并 ====================
//
// 背景：对账原本是「每 5 分钟拉一次上游账单 + 批量匹配」。改成实时之后，每发现
// 新的下游扣费记录就安排一次「拉一小段账单并立刻匹配」。这条路有两个能把生产
// 打坏的地方，本文件专门把它们钉死：
//
//  1. 通知发生在记账完成之后。只要它有可能阻塞，就会把请求链路一起拖住——
//     丢一次通知只是晚一点对账，阻塞一次请求是事故。
//  2. 一次压测能产生上千条调用。若每条都去拉一次上游，上游必然限流，
//     结果是对账没变快、上游却被自家打挂。
//
// 因此断言只有两条：通道塞满时通知依旧立刻返回；密集通知只换来一次上游拉取。

// realtimeBillRepoStub 记录 staging 查询次数，用来证明实时轮确实走了匹配。
type realtimeBillRepoStub struct {
	ReconciliationUpstreamBillRepository

	upserts     int
	listStaging int
}

func (r *realtimeBillRepoStub) UpsertBatch(_ context.Context, _ []ReconciliationUpstreamBillPayload) (int64, error) {
	r.upserts++
	return 0, nil
}

func (r *realtimeBillRepoStub) ListStaging(_ context.Context, _, _ time.Time, _ int) ([]ReconciliationUpstreamBill, error) {
	r.listStaging++
	return nil, nil
}

// newRealtimeCollectorHarness 组装一个「只跑实时线」的采集器。
//
// 两个周期都被拉到 1 小时：本测试只关心实时循环，定时线一旦插进来就会让
// 「上游被拉了几次」这个断言失去意义。
func newRealtimeCollectorHarness(cfg ReconciliationCollectorConfig) (*ReconciliationCollector, *billSyncSourceStub, *realtimeBillRepoStub) {
	// 上游必须真的返回一条账单：否则实时轮在 FetchBills 之后就走空导入，
	// upserts / listStaging 都会是 0，断言就失去意义了。
	source := &billSyncSourceStub{bills: []ReconciliationUpstreamBillPayload{billSyncPayload("rt-realtime-1")}}
	repo := &realtimeBillRepoStub{}
	svc := NewReconciliationSyncService(
		&collectCursorExtrasRepo{},
		repo,
		&collectCursorRuleRepo{},
		newCollectCursorStateRepo(),
		nil,
		source,
		ReconciliationSyncConfig{
			FxUSDCNYRate:     1,
			A6Lookback:       24 * time.Hour,
			CollectBatchSize: 100,
			StagingLimit:     100,
		},
	)

	if cfg.UsageInterval <= 0 {
		cfg.UsageInterval = time.Hour
	}
	if cfg.BillInterval <= 0 {
		cfg.BillInterval = time.Hour
	}
	// Enabled 必须为真，否则 Start() 直接返回，连实时循环都不会起。
	// RealtimeEnabled 由各用例自己给：本文件既有「启用实时」也有「禁用实时」两种断言。
	cfg.Enabled = true
	if cfg.RealtimeWindow <= 0 {
		cfg.RealtimeWindow = 15 * time.Minute
	}

	// 不调用 SetLeaderLock：采集器会退化成「单实例假设」，锁恒为获取成功。
	// 这正是本测试想要的——我们要验的是调度，不是抢锁。
	return NewReconciliationCollector(svc, cfg), source, repo
}

// TestReconciliationCollector_NotifyUsageRecordedNeverBlocks 通道被塞满之后，
// 后续通知必须依旧立刻返回。这是「通知在请求链路上」这条约束的底线。
func TestReconciliationCollector_NotifyUsageRecordedNeverBlocks(t *testing.T) {
	collector, _, _ := newRealtimeCollectorHarness(ReconciliationCollectorConfig{
		RealtimeEnabled: true,
	})

	// 先塞满容量为 1 的通道，再打 500 次：任何一次阻塞都会让本测试超时。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			collector.NotifyUsageRecorded()
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "NotifyUsageRecorded 阻塞了：它在请求链路上，绝不允许阻塞调用方")
	}
}

// TestReconciliationCollector_NotifyUsageRecordedNoopWhenDisabled 未启用实时匹配时，
// 通知必须完全空转：既不投递也不阻塞，调用方不需要判空、不需要分支。
func TestReconciliationCollector_NotifyUsageRecordedNoopWhenDisabled(t *testing.T) {
	collector, _, _ := newRealtimeCollectorHarness(ReconciliationCollectorConfig{
		RealtimeEnabled: false,
	})

	collector.NotifyUsageRecorded()

	assert.Empty(t, collector.realtimeCh, "未启用实时匹配时不应该投递任何通知")
}

// TestReconciliationCollector_NotifyUsageRecordedNilReceiverSafe 空接收者必须安全：
// 采集器可能因为对账功能整体关闭而根本没被构造出来。
func TestReconciliationCollector_NotifyUsageRecordedNilReceiverSafe(t *testing.T) {
	var collector *ReconciliationCollector

	assert.NotPanics(t, func() { collector.NotifyUsageRecorded() })
}

// TestNewReconciliationCollector_RealtimeDefaults 零值配置必须落到安全默认值上。
//
// 尤其是 RealtimeDelay：它是 0 的话，发现调用就立刻去拉上游，而上游账单通常
// 还没落库，结果是「每次都拉一次、每次都拉不到」，既浪费配额又看起来像坏了。
func TestNewReconciliationCollector_RealtimeDefaults(t *testing.T) {
	collector := NewReconciliationCollector(nil, ReconciliationCollectorConfig{})

	assert.Equal(t, 30*time.Second, collector.cfg.RealtimeDelay)
	assert.Equal(t, time.Minute, collector.cfg.RealtimeMinInterval)
	assert.Equal(t, 15*time.Minute, collector.cfg.RealtimeWindow)
	require.NotNil(t, collector.realtimeCh, "必须预建通知通道，否则第一次通知会 panic")
	assert.Equal(t, 1, cap(collector.realtimeCh), "通道容量必须是 1：它传递的是「有新记录」这个状态而不是记录条数")
}

// TestReconciliationCollector_RealtimeLoopCoalescesAndThrottles 是实时线的核心断言：
//
//	① 密集通知只换来一次上游拉取（合并）；
//	② 那一轮确实跑完了「拉账单 + 匹配」两件事；
//	③ 最小间隔内再次通知不会立刻再打一次上游（限流兜底）。
func TestReconciliationCollector_RealtimeLoopCoalescesAndThrottles(t *testing.T) {
	collector, source, repo := newRealtimeCollectorHarness(ReconciliationCollectorConfig{
		RealtimeEnabled:     true,
		RealtimeDelay:       30 * time.Millisecond,
		RealtimeMinInterval: 10 * time.Second,
		RealtimeWindow:      15 * time.Minute,
	})

	collector.Start()
	defer collector.Stop()

	// 50 条「刚产生的扣费记录」几乎同时到达。
	for i := 0; i < 50; i++ {
		collector.NotifyUsageRecorded()
	}

	require.Eventually(t, func() bool { return source.calls >= 1 }, 3*time.Second, 5*time.Millisecond,
		"实时循环没有在上游拉取账单")
	assert.Equal(t, 1, source.calls, "密集通知必须被合并成一次上游拉取")
	assert.Equal(t, 1, repo.upserts, "实时轮只应该导入一次账单")
	assert.Equal(t, 1, repo.listStaging, "实时轮必须接着跑一次匹配，否则只是白拉账单")

	// 最小间隔内再来一波：上游不应该被再打一次。
	collector.NotifyUsageRecorded()
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, source.calls, "最小间隔内不得再次打上游，否则密集流量会把上游打成限流")
}

// TestReconciliationCollector_StopWaitsForRealtimeLoop Stop() 必须等实时循环退出，
// 否则进程收尾时会出现「日志还在写、依赖已经拆掉」的竞态。
func TestReconciliationCollector_StopWaitsForRealtimeLoop(t *testing.T) {
	collector, _, _ := newRealtimeCollectorHarness(ReconciliationCollectorConfig{
		RealtimeEnabled: true,
		RealtimeDelay:   time.Millisecond,
	})

	collector.Start()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		collector.Stop()
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Stop 没有等到实时循环退出")
	}
}
