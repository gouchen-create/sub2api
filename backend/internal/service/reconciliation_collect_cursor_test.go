//go:build unit

package service

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 采集游标：不越过未采集行 ====================
//
// 这组测试锁死 P0 事故的根因：采集器单轮有上限（CollectBatchSize），
// 旧实现不管本轮有没有取满，都把游标无条件写成 now。于是被上限截掉的那部分行
// 再也不会被任何一轮扫到——它们的下游收入永久记 0，却照样会被匹配到上游账单，
// 直接算出负毛利。线上/dev 实测：45492 行里 39975 行没有快照，而游标停在
// 所有剩余行之后（重扫带内 0 行），等待再久也不会补上。
//
// 关键背景（dev 库实测）：一个批次的 5000 行常常只跨越 ~1 毫秒，
// 平均每微秒有 7 行共享同一个 created_at。所以「游标推进到最后一行的时间」
// 这种朴素做法会在同一时间戳上原地打转，必须用 (created_at, id) 复合游标。

// collectCursorUsageSource 用内存数据模拟 ListUsageBetween / CountUsagePending 的 SQL 语义。
//
// 刻意逐条实现谓词而不是简化成切片切割：测试要证明的正是谓词语义
// （尤其是 created_at <> 边界 OR id > 水位 这一条），简化掉就等于没测。
type collectCursorUsageSource struct {
	ReconciliationUsageSource

	facts []ReconciliationUsageFact

	// rounds 记录每轮真正返回的行，供断言「轮次之间不重复、合起来不漏」。
	rounds [][]int64
	// windows 记录每轮的查询条件，供断言游标回退量。
	windows []ReconciliationUsageQuery
	// listErr / countErr 用于注入错误路径。
	listErr  error
	countErr error
}

func (s *collectCursorUsageSource) ListUsageBetween(_ context.Context, query ReconciliationUsageQuery) ([]ReconciliationUsageFact, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.windows = append(s.windows, query)

	limit := query.Limit
	if limit <= 0 {
		limit = 5000
	}

	out := make([]ReconciliationUsageFact, 0, limit)
	for _, fact := range s.facts {
		if fact.CreatedAt.Before(query.From) || !fact.CreatedAt.Before(query.To) {
			continue
		}
		// 已采集过的位置：created_at == 边界且 id <= 水位。其余一律返回
		// （含 created_at < 边界的重扫带）。
		if fact.CreatedAt.Equal(query.BoundaryAt) && fact.UsageLogID <= query.BoundaryID {
			continue
		}
		out = append(out, fact)
		if len(out) >= limit {
			break
		}
	}

	ids := make([]int64, 0, len(out))
	for _, fact := range out {
		ids = append(ids, fact.UsageLogID)
	}
	s.rounds = append(s.rounds, ids)
	return out, nil
}

func (s *collectCursorUsageSource) CountUsagePending(_ context.Context, query ReconciliationUsageQuery, limit int64) (int64, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	if limit <= 0 {
		limit = 1
	}
	var count int64
	for _, fact := range s.facts {
		if fact.CreatedAt.Before(query.From) || !fact.CreatedAt.Before(query.To) {
			continue
		}
		if fact.CreatedAt.Equal(query.BoundaryAt) && fact.UsageLogID <= query.BoundaryID {
			continue
		}
		count++
		if count >= limit {
			break
		}
	}
	return count, nil
}

// collectCursorExtrasRepo 记录写进快照表的调用主键，并按唯一索引语义去重。
type collectCursorExtrasRepo struct {
	ReconciliationUsageExtraRepository

	written   []int64
	seen      map[int64]struct{}
	upsertErr error
}

func (r *collectCursorExtrasRepo) UpsertBatch(_ context.Context, extras []ReconciliationUsageExtra) (int64, error) {
	if r.upsertErr != nil {
		return 0, r.upsertErr
	}
	if r.seen == nil {
		r.seen = make(map[int64]struct{}, len(extras))
	}
	var inserted int64
	for _, extra := range extras {
		if _, exists := r.seen[extra.UsageLogID]; exists {
			continue
		}
		r.seen[extra.UsageLogID] = struct{}{}
		r.written = append(r.written, extra.UsageLogID)
		inserted++
	}
	return inserted, nil
}

// collectCursorStateRepo 是状态表的内存实现。
type collectCursorStateRepo struct {
	ReconciliationSyncStateRepository

	values map[string]string
	setErr map[string]error
}

func newCollectCursorStateRepo() *collectCursorStateRepo {
	return &collectCursorStateRepo{values: make(map[string]string)}
}

func (r *collectCursorStateRepo) Get(_ context.Context, key string) (string, error) {
	if err := r.setErr[key]; err != nil {
		return "", err
	}
	return r.values[key], nil
}

func (r *collectCursorStateRepo) Set(_ context.Context, key, value string) error {
	if err := r.setErr[key]; err != nil {
		return err
	}
	r.values[key] = value
	return nil
}

func (r *collectCursorStateRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := r.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// collectCursorRuleRepo 提供空规则集：本组测试只关心采集推进，不关心规则快照。
type collectCursorRuleRepo struct {
	ReconciliationAccountRuleRepository
}

func (r *collectCursorRuleRepo) List(_ context.Context) ([]ReconciliationAccountRule, error) {
	return nil, nil
}

// newCollectCursorHarness 组装一套采集器依赖。
func newCollectCursorHarness(batchSize int, facts []ReconciliationUsageFact) (*ReconciliationSyncService, *collectCursorUsageSource, *collectCursorExtrasRepo, *collectCursorStateRepo) {
	source := &collectCursorUsageSource{facts: facts}
	extras := &collectCursorExtrasRepo{}
	state := newCollectCursorStateRepo()
	svc := NewReconciliationSyncService(
		extras,
		nil,
		&collectCursorRuleRepo{},
		state,
		source,
		nil,
		ReconciliationSyncConfig{
			FxUSDCNYRate:     1,
			A6Lookback:       time.Hour,
			CollectBatchSize: batchSize,
		},
	)
	return svc, source, extras, state
}

// collectCursorFact 造一条调用事实。
func collectCursorFact(id int64, at time.Time) ReconciliationUsageFact {
	return ReconciliationUsageFact{UsageLogID: id, AccountID: 1, ActualCost: 0.001, CreatedAt: at}
}

// assertCursorNeverCrossedUncollected 断言不变量：
//
//	对任何尚未写进快照的行 r，都不允许 r 的位置 <= 游标位置（按 (created_at, id) 比较）。
//
// 这是「游标绝不越过未采集行」的直接形式化。
func assertCursorNeverCrossedUncollected(t *testing.T, cursorAt time.Time, cursorID int64, facts []ReconciliationUsageFact, collected map[int64]struct{}) {
	t.Helper()

	for _, fact := range facts {
		if _, ok := collected[fact.UsageLogID]; ok {
			continue
		}
		crossed := fact.CreatedAt.Before(cursorAt) ||
			(fact.CreatedAt.Equal(cursorAt) && fact.UsageLogID <= cursorID)
		assert.Falsef(t, crossed,
			"游标 (%s, %d) 越过了未采集的行 id=%d created_at=%s：这一行将永远不会被采集",
			cursorAt.Format(time.RFC3339Nano), cursorID, fact.UsageLogID, fact.CreatedAt.Format(time.RFC3339Nano))
	}
}

// readCursor 读取状态表里的复合游标。
func readCursor(t *testing.T, state *collectCursorStateRepo) (time.Time, int64) {
	t.Helper()

	rawAt := state.values[ReconciliationStateKeyUsageCursor]
	require.NotEmpty(t, rawAt, "采集完成后必须写下 usage_last_collected_at")
	at, err := time.Parse(time.RFC3339Nano, rawAt)
	require.NoError(t, err, "usage_last_collected_at 必须是 RFC3339Nano")

	rawID := state.values[ReconciliationStateKeyUsageCursorID]
	require.NotEmpty(t, rawID, "采集完成后必须写下 usage_last_collected_id")
	id, err := strconv.ParseInt(rawID, 10, 64)
	require.NoError(t, err, "usage_last_collected_id 必须是十进制整数")
	return at, id
}

// TestCollectUsage_SameTimestampBulkIsDrainedWithoutLoss 是 P0 的核心回归测试。
//
// 12 行共享**同一个 created_at**（dev 库的真实形态：一个批次 5000 行只跨 ~1ms），
// 单轮上限 5，数据落在 10 分钟前（批量导入/回填的常见形态）。
// 期望：三轮 5 + 5 + 2 把 12 行全部写进快照，游标始终停在已采集位置，
// 既不漏行、也不在同一时间戳上无限打转。
//
// 在修复前的代码上这个测试必然失败：第一轮取满 5 行后游标被写成 now，
// 第二轮窗口 [now-1min, now) 里没有这些 10 分钟前的行，于是采集中断，
// 剩下 7 行永久丢失（断言 collected 长度时会直接报 5 != 12）。
func TestCollectUsage_SameTimestampBulkIsDrainedWithoutLoss(t *testing.T) {
	now := time.Now().UTC()
	bulkAt := now.Add(-10 * time.Minute)

	facts := make([]ReconciliationUsageFact, 0, 12)
	for id := int64(1); id <= 12; id++ {
		facts = append(facts, collectCursorFact(id, bulkAt))
	}

	svc, source, extras, state := newCollectCursorHarness(5, facts)

	collected := make(map[int64]struct{}, len(facts))
	rounds := 0
	for round := 0; round < 8; round++ {
		inserted, err := svc.CollectUsage(context.Background())
		require.NoError(t, err)
		rounds++

		for _, id := range source.rounds[len(source.rounds)-1] {
			collected[id] = struct{}{}
		}

		cursorAt, cursorID := readCursor(t, state)
		assertCursorNeverCrossedUncollected(t, cursorAt, cursorID, facts, collected)

		if inserted == 0 && len(source.rounds[len(source.rounds)-1]) == 0 {
			break
		}
	}

	require.Len(t, collected, 12, "12 行必须全部被采集到，一行都不能漏")
	require.Len(t, extras.written, 12, "快照表必须收到全部 12 行")
	assert.Equal(t, 4, rounds, "12 行 / 单轮 5 行 = 3 个采集轮 + 1 个确认轮；既不能漏行，也不能在同一时间戳上卡死")

	// 轮次之间不能重复取同一批（重复 = 游标没前进）。
	require.Len(t, source.rounds, 4)
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, source.rounds[0])
	assert.Equal(t, []int64{6, 7, 8, 9, 10}, source.rounds[1])
	assert.Equal(t, []int64{11, 12}, source.rounds[2])
	assert.Empty(t, source.rounds[3], "第 4 轮是确认轮：已经没有待采集的行")
}

// TestCollectUsage_TruncatedRoundStopsAtLastCollectedRow 断言被上限截断时的推进规则：
// 游标只能落到本批**最后一行**的 (created_at, id)，绝不允许跳到 now。
func TestCollectUsage_TruncatedRoundStopsAtLastCollectedRow(t *testing.T) {
	now := time.Now().UTC()
	bulkAt := now.Add(-10 * time.Minute)

	facts := make([]ReconciliationUsageFact, 0, 7)
	for id := int64(101); id <= 107; id++ {
		facts = append(facts, collectCursorFact(id, bulkAt))
	}

	svc, _, _, state := newCollectCursorHarness(5, facts)

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 5, inserted)

	cursorAt, cursorID := readCursor(t, state)
	assert.True(t, cursorAt.Equal(bulkAt),
		"被截断时游标必须停在本批最后一行的 created_at，实际 %s", cursorAt.Format(time.RFC3339Nano))
	assert.EqualValues(t, 105, cursorID, "同一时刻上的主键水位必须落在本批最大 id")

	// 旧实现会把游标写到 now：这里显式钉死「不许越过重扫带」。
	assert.True(t, cursorAt.Before(now.Add(-reconciliationCollectOverlap)),
		"游标不得跳到 now（本次数据在 10 分钟前，跳到 now 就等于丢掉剩余 2 行）")

	// 状态表里的可观测字段：本轮被上限塞满时必须能查出来。
	assert.Equal(t, "5", state.values[ReconciliationStateKeyUsageBatchSize])
	assert.Equal(t, "true", state.values[ReconciliationStateKeyUsageTruncated])
	assert.Equal(t, "2", state.values[ReconciliationStateKeyUsageBacklog],
		"积压行数必须暴露出来：被截断时还剩 2 行没采")
}

// TestCollectUsage_UntruncatedRoundKeepsOverlapBand 断言正常路径（没取满）下的推进：
// 可以往前走，但要留在 now-重叠量 上，保住那 1 分钟的重扫带。
func TestCollectUsage_UntruncatedRoundKeepsOverlapBand(t *testing.T) {
	now := time.Now().UTC()
	facts := []ReconciliationUsageFact{
		collectCursorFact(1, now.Add(-30*time.Second)),
		collectCursorFact(2, now.Add(-20*time.Second)),
	}

	svc, _, _, state := newCollectCursorHarness(5, facts)

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, inserted)

	cursorAt, cursorID := readCursor(t, state)
	assert.EqualValues(t, 0, cursorID, "没被截断时水位归零：该时刻的行下一轮整批重扫，幂等写入不会产生重复行")
	assert.WithinDuration(t, now.Add(-reconciliationCollectOverlap), cursorAt, 2*time.Second,
		"未截断时应推进到 now-重叠量：既按轮前移（文档 15.1 靠它判断采集器活着），又保留一分钟重扫带")

	assert.Equal(t, "2", state.values[ReconciliationStateKeyUsageBatchSize])
	assert.Equal(t, "false", state.values[ReconciliationStateKeyUsageTruncated])
	assert.Equal(t, "0", state.values[ReconciliationStateKeyUsageBacklog], "没被截断时积压必须清零")
}

// TestCollectUsage_EmptyRoundStillAdvancesCursor 保证空闲站点上游标继续前移。
//
// 旧实现在没有新调用时直接 return，不写游标；文档 15.1 用 usage_last_collected_at
// 判断采集器是否存活，游标长期不动会被误判成采集器挂了。
func TestCollectUsage_EmptyRoundStillAdvancesCursor(t *testing.T) {
	now := time.Now().UTC()
	svc, _, _, state := newCollectCursorHarness(5, nil)

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 0, inserted)

	cursorAt, cursorID := readCursor(t, state)
	assert.WithinDuration(t, now.Add(-reconciliationCollectOverlap), cursorAt, 2*time.Second)
	assert.EqualValues(t, 0, cursorID)
	assert.Equal(t, "0", state.values[ReconciliationStateKeyUsageBatchSize])
	assert.Equal(t, "false", state.values[ReconciliationStateKeyUsageTruncated])
}

// TestCollectUsage_RescansOverlapBandForLateCommits 保证游标回退出来的重扫带仍然有效：
// 上一轮之后才落库、created_at 落在游标之前一分钟内的行必须还能被采到。
func TestCollectUsage_RescansOverlapBandForLateCommits(t *testing.T) {
	now := time.Now().UTC()
	first := collectCursorFact(1, now.Add(-3*time.Minute))

	svc, source, extras, state := newCollectCursorHarness(10, []ReconciliationUsageFact{first})

	_, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	cursorAt, _ := readCursor(t, state)
	require.True(t, cursorAt.After(first.CreatedAt.Add(-reconciliationCollectOverlap)))

	// 迟到落库：created_at 比游标早，但落在重扫带里（游标 - 1 分钟之后）。
	lateAt := cursorAt.Add(-30 * time.Second)
	source.facts = append(source.facts, collectCursorFact(2, lateAt))

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 1, inserted, "重扫带内的迟到行必须被补采")
	assert.Contains(t, extras.written, int64(2))

	// 再跑一轮：唯一索引语义下不会重复写入。
	inserted, err = svc.CollectUsage(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 0, inserted, "重复扫描同一区间不得产生重复行")
	assert.Equal(t, 1, countOccurrences(extras.written, 2))
}

// TestCollectUsage_LegacyCursorWithoutIDRescansSameTimestamp 覆盖升级路径：
// 状态表里只有旧的时间游标（没有 id）时，同一时刻的行必须整批重扫，
// 绝不能因为「水位缺失」被当成已采集而跳过。
func TestCollectUsage_LegacyCursorWithoutIDRescansSameTimestamp(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-2 * time.Minute)

	svc, _, extras, state := newCollectCursorHarness(10, []ReconciliationUsageFact{
		collectCursorFact(1, at),
		collectCursorFact(2, at),
	})

	// 模拟旧版本留下的状态：只有时间，没有主键水位。
	state.values[ReconciliationStateKeyUsageCursor] = at.Format(time.RFC3339Nano)

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 2, inserted, "旧游标下的同一时间戳必须整批重扫，不能漏")
	assert.Equal(t, []int64{1, 2}, extras.written)
}

// TestCollectUsage_CorruptCursorIDFallsBackToZero 主键水位读坏时按 0 处理：
// 代价是该时刻整批重扫一次（幂等），而不是静默漏行。
func TestCollectUsage_CorruptCursorIDFallsBackToZero(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-2 * time.Minute)

	svc, _, _, state := newCollectCursorHarness(10, []ReconciliationUsageFact{
		collectCursorFact(1, at),
	})
	state.values[ReconciliationStateKeyUsageCursor] = at.Format(time.RFC3339Nano)
	state.values[ReconciliationStateKeyUsageCursorID] = "not-a-number"

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 1, inserted)

	_, cursorID := readCursor(t, state)
	assert.GreaterOrEqual(t, cursorID, int64(0))
}

// TestCollectUsage_UpsertFailureDoesNotAdvanceCursor 写入失败时游标必须原地不动，
// 下一轮重扫同一区间（快照表唯一索引保证幂等）。
func TestCollectUsage_UpsertFailureDoesNotAdvanceCursor(t *testing.T) {
	now := time.Now().UTC()
	svc, _, extras, state := newCollectCursorHarness(10, []ReconciliationUsageFact{
		collectCursorFact(1, now.Add(-time.Minute)),
	})
	extras.upsertErr = assert.AnError

	_, err := svc.CollectUsage(context.Background())
	require.Error(t, err)
	assert.Empty(t, state.values[ReconciliationStateKeyUsageCursor],
		"写入失败时不得推进游标，否则这一段调用永久丢失")
}

// TestAdvanceUsageCursor_IsMonotonicOnSameTimestamp 复合游标在同一时间戳上必须严格前进。
//
// 这是「同一 created_at 上的行数超过单轮上限也不会死循环」的形式化证明：
// 每轮的水位都严格大于上一轮，批次必然收敛。
func TestAdvanceUsageCursor_IsMonotonicOnSameTimestamp(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 30, 0, 0, time.UTC)
	batch := func(lastID int64) []ReconciliationUsageFact {
		return []ReconciliationUsageFact{collectCursorFact(lastID, at)}
	}

	svc, _, _, state := newCollectCursorHarness(1, nil)
	ctx := context.Background()

	prev := reconciliationUsageCursor{At: at.Add(-reconciliationCollectOverlap)}
	var lastID int64
	for _, id := range []int64{10, 20, 30, 40} {
		next, err := svc.advanceUsageCursor(ctx, prev, batch(id), at.Add(time.Minute), true)
		require.NoError(t, err)
		require.Equal(t, at, next.At)
		assert.Greater(t, next.ID, lastID, "同一时间戳上的主键水位必须严格递增")
		lastID = next.ID
		prev = next
	}

	raw, err := state.Get(ctx, ReconciliationStateKeyUsageCursorID)
	require.NoError(t, err)
	assert.Equal(t, "40", raw)
}

// TestCollectUsage_BackdatedBulkSpansManyRounds 用「一批一行」的极端上限跑通全过程：
// 20 行共享一个时间戳、单轮上限 1，必须是 20 轮全部采完，一轮都不能跳过。
func TestCollectUsage_BackdatedBulkSpansManyRounds(t *testing.T) {
	now := time.Now().UTC()
	bulkAt := now.Add(-30 * time.Minute)

	facts := make([]ReconciliationUsageFact, 0, 20)
	for id := int64(1); id <= 20; id++ {
		facts = append(facts, collectCursorFact(id, bulkAt))
	}

	svc, source, extras, state := newCollectCursorHarness(1, facts)

	collected := make(map[int64]struct{}, len(facts))
	for round := 0; round < 25; round++ {
		_, err := svc.CollectUsage(context.Background())
		require.NoError(t, err)

		for _, id := range source.rounds[len(source.rounds)-1] {
			collected[id] = struct{}{}
		}
		cursorAt, cursorID := readCursor(t, state)
		assertCursorNeverCrossedUncollected(t, cursorAt, cursorID, facts, collected)
	}

	require.Len(t, collected, 20, "单轮上限 1 也必须把 20 行全部采完")
	require.Len(t, extras.written, 20)

	// 前 20 轮每轮恰好推进一行（不许重复取同一批），之后的轮次都是空轮。
	require.GreaterOrEqual(t, len(source.rounds), 20)
	for i := 0; i < 20; i++ {
		assert.Len(t, source.rounds[i], 1, "第 %d 轮必须恰好取到一行（水位严格前进）", i+1)
		assert.Equal(t, int64(i+1), source.rounds[i][0])
	}

	// （额外自检）collectCursorFact 产生的行按 (created_at, id) 有序，
	// 与 SQL 的 ORDER BY created_at, id 一致。
	ids := make([]int64, 0, len(facts))
	for _, fact := range facts {
		ids = append(ids, fact.UsageLogID)
	}
	assert.True(t, sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }))
}

// TestCollectBacklog_ReportsPendingRows 只读诊断接口：不改状态、只数积压。
func TestCollectBacklog_ReportsPendingRows(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-5 * time.Minute)

	svc, _, _, state := newCollectCursorHarness(10, []ReconciliationUsageFact{
		collectCursorFact(1, at),
		collectCursorFact(2, at),
		collectCursorFact(3, at),
	})
	state.values[ReconciliationStateKeyUsageCursor] = at.Add(-time.Minute).Format(time.RFC3339Nano)
	state.values[ReconciliationStateKeyUsageCursorID] = "0"

	pending, err := svc.CollectBacklog(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 3, pending)
	assert.Empty(t, state.values[ReconciliationStateKeyUsageBatchSize], "只读诊断不得写状态表")
}

// TestCollectUsage_BacklogProbeFailureDoesNotBreakCollection 积压探针失败不能影响采集本身。
func TestCollectUsage_BacklogProbeFailureDoesNotBreakCollection(t *testing.T) {
	now := time.Now().UTC()
	bulkAt := now.Add(-10 * time.Minute)

	facts := make([]ReconciliationUsageFact, 0, 3)
	for id := int64(1); id <= 3; id++ {
		facts = append(facts, collectCursorFact(id, bulkAt))
	}

	svc, source, extras, state := newCollectCursorHarness(2, facts)
	source.countErr = assert.AnError

	inserted, err := svc.CollectUsage(context.Background())
	require.NoError(t, err, "探针失败不能把采集整轮判为失败")
	assert.EqualValues(t, 2, inserted)
	assert.Equal(t, "true", state.values[ReconciliationStateKeyUsageTruncated])
	assert.Empty(t, state.values[ReconciliationStateKeyUsageBacklog], "探针失败时不写积压数字，避免展示错的数")
	assert.Len(t, extras.written, 2)
}

// countOccurrences 数一个切片里某个值出现几次。
func countOccurrences(values []int64, target int64) int {
	var count int
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}
