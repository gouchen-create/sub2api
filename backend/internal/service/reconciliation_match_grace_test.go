//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 孤儿宽限期：从「导入时刻」起算 ====================
//
// 文档 6.3 写的是「账单导入后有宽限期（默认 30 分钟）才会被判定为孤儿」。
// 旧实现拿 bill.Payload.OccurredAt（上游流水发生时间）当起点，于是首次拉取
// 24 小时窗口时，每条账单的 occurred_at 早就超过 30 分钟——宽限期等于不存在，
// 整批刚导入的账单在第一轮就被判成孤儿。
//
// 这些测试锁死两件事：
//  1. 起点必须是 imported_at，不是 occurred_at；
//  2. 真孤儿（导入也超过宽限期）仍然要被判成孤儿，不能因为改口径而永远留在 staging。

// graceTestBillRepo 只实现宽限期判定需要的两个方法。
type graceTestBillRepo struct {
	ReconciliationUpstreamBillRepository

	staging     []ReconciliationUpstreamBill
	unmatched   []int64
	markErr     error
	lastFrom    time.Time
	lastTo      time.Time
	lastLimit   int
	listStaging int
}

func (r *graceTestBillRepo) ListStaging(_ context.Context, from, to time.Time, limit int) ([]ReconciliationUpstreamBill, error) {
	r.listStaging++
	r.lastFrom, r.lastTo, r.lastLimit = from, to, limit
	return r.staging, nil
}

func (r *graceTestBillRepo) MarkUnmatched(_ context.Context, ids []int64) error {
	if r.markErr != nil {
		return r.markErr
	}
	r.unmatched = append(r.unmatched, ids...)
	return nil
}

// graceTestExtrasRepo 提供空的令牌反查（本组测试不需要历史快照）。
type graceTestExtrasRepo struct {
	ReconciliationUsageExtraRepository
}

func (r *graceTestExtrasRepo) ListAccountIDsByRuleKeys(_ context.Context, _ []string) ([]int64, error) {
	return nil, nil
}

// newGraceHarness 组装一个只关心孤儿判定的服务。
func newGraceHarness(bills []ReconciliationUpstreamBill) (*ReconciliationSyncService, *graceTestBillRepo) {
	billRepo := &graceTestBillRepo{staging: bills}
	svc := NewReconciliationSyncService(
		&graceTestExtrasRepo{},
		billRepo,
		&collectCursorRuleRepo{},
		newCollectCursorStateRepo(),
		nil,
		nil,
		ReconciliationSyncConfig{
			FxUSDCNYRate:     1,
			A6Lookback:       24 * time.Hour,
			CollectBatchSize: 100,
			StagingLimit:     100,
			MatchGracePeriod: 30 * time.Minute,
		},
	)
	return svc, billRepo
}

// graceTestBill 造一条「上游流水很旧、刚刚才导入」的账单。
func graceTestBill(id int64, occurredAt, importedAt time.Time) ReconciliationUpstreamBill {
	return ReconciliationUpstreamBill{
		ID:         id,
		ImportedAt: importedAt,
		Payload: ReconciliationUpstreamBillPayload{
			Provider:          ReconciliationProviderA6,
			UpstreamRequestID: "req-grace",
			OccurredAt:        occurredAt,
			Model:             "claude-sonnet-4.5",
			TokenName:         "token-grace",
			OutputTokens:      10,
		},
	}
}

// TestMatchStaging_GracePeriodStartsAtImportNotOccurredAt 是 P1-2 的核心回归测试。
//
// 账单的上游流水发生在 30 天前（首次拉取 24 小时窗口的真实形态），
// 但 1 分钟前才导入本站 ⇒ 宽限期还没走完，**不得**判定为孤儿。
//
// 在修复前的代码上这个测试必然失败：旧口径拿 occurred_at 当起点，
// 30 天前 > 30 分钟，账单第一轮就被 MarkUnmatched。
func TestMatchStaging_GracePeriodStartsAtImportNotOccurredAt(t *testing.T) {
	now := time.Now().UTC()
	bill := graceTestBill(1, now.Add(-30*24*time.Hour), now.Add(-time.Minute))

	svc, billRepo := newGraceHarness([]ReconciliationUpstreamBill{bill})

	matched, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, matched)
	assert.EqualValues(t, 0, orphaned, "刚导入 1 分钟的账单不能因为上游流水久远就被判成孤儿")
	assert.Empty(t, billRepo.unmatched)
}

// TestMatchStaging_GenuineOrphanIsStillMarked 真孤儿（导入也超过宽限期）继续判定为孤儿。
//
// 改口径不能把孤儿判定整体废掉：文档 17.7 明确孤儿不会自动重试，
// 一直留在 staging 会让每一轮匹配都重复扫同一批账单。
func TestMatchStaging_GenuineOrphanIsStillMarked(t *testing.T) {
	now := time.Now().UTC()
	bill := graceTestBill(7, now.Add(-3*24*time.Hour), now.Add(-2*time.Hour))

	svc, billRepo := newGraceHarness([]ReconciliationUpstreamBill{bill})

	_, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, orphaned)
	assert.Equal(t, []int64{7}, billRepo.unmatched)
}

// TestMatchStaging_GraceBoundaryUsesImportTime 边界值：导入刚过宽限期就要判孤儿，
// 差一点点则继续等待。这条断言防止把「起点」又写回 occurred_at 之类的字段。
func TestMatchStaging_GraceBoundaryUsesImportTime(t *testing.T) {
	now := time.Now().UTC()

	t.Run("导入 31 分钟：判定为孤儿", func(t *testing.T) {
		bill := graceTestBill(11, now.Add(-10*24*time.Hour), now.Add(-31*time.Minute))
		svc, billRepo := newGraceHarness([]ReconciliationUpstreamBill{bill})

		_, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-24*time.Hour), now)
		require.NoError(t, err)
		assert.EqualValues(t, 1, orphaned)
		assert.Equal(t, []int64{11}, billRepo.unmatched)
	})

	t.Run("导入 29 分钟：继续等待", func(t *testing.T) {
		bill := graceTestBill(12, now.Add(-10*24*time.Hour), now.Add(-29*time.Minute))
		svc, billRepo := newGraceHarness([]ReconciliationUpstreamBill{bill})

		_, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-24*time.Hour), now)
		require.NoError(t, err)
		assert.EqualValues(t, 0, orphaned)
		assert.Empty(t, billRepo.unmatched)
	})
}

// TestGraceBase_FallsBackWhenImportedAtMissing 实现方没给出入库时间时退回 occurred_at。
//
// 这是兼容兜底而不是正确口径：一旦退回，宽限期就退化成旧行为。
// 保留它是为了不让「字段缺失」直接导致孤儿判定彻底失效（账单会永远留在 staging）。
func TestGraceBase_FallsBackWhenImportedAtMissing(t *testing.T) {
	now := time.Now().UTC()
	occurred := now.Add(-2 * time.Hour)

	bill := graceTestBill(21, occurred, time.Time{})
	assert.True(t, graceBase(&bill).Equal(occurred), "入库时间缺失时退回上游流水时间")

	imported := now.Add(-time.Minute)
	bill.ImportedAt = imported
	assert.True(t, graceBase(&bill).Equal(imported), "有入库时间时必须以它为准")

	assert.True(t, graceBase(nil).IsZero())
}

// TestMatchStaging_EmptyStagingIsNoOp 空批次必须早退：不查规则、不写状态。
func TestMatchStaging_EmptyStagingIsNoOp(t *testing.T) {
	now := time.Now().UTC()
	svc, billRepo := newGraceHarness(nil)

	matched, orphaned, err := svc.MatchStaging(context.Background(), now.Add(-time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, matched)
	assert.EqualValues(t, 0, orphaned)
	assert.Equal(t, 1, billRepo.listStaging)
	assert.Empty(t, billRepo.unmatched)
}
