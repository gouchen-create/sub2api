//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 孤儿账单的显式重试通道 ====================
//
// 缺陷事实（生产库确证）：ListStaging 的谓词是 match_state='staging'，
// 而 MarkUnmatched 会把账单置为 'unmatched' ⇒ 账单一旦被判成孤儿就再也不会被捞出来。
// 生产实测 staging=37 / unmatched=3500 / matched=0：管理员把 3 个填错的令牌名改对之后，
// 本可匹配上的 2487 条全部卡在 unmatched 里，改规则不会让它们复活。
//
// 这些测试锁死重试方法的三条语义：
//  1. 只退回「当前规则能解析出账号」的孤儿（不无差别退回，也不碰 staging/matched）；
//  2. 退回时重置宽限期起算点（否则老账单下一轮立刻又被判回孤儿，等于白做）；
//  3. 单批上限与单轮匹配上限一致，保证退回的这批能被随后的一次匹配覆盖。

// requeueBillState 与数据库里的 match_state 取值一致。
type requeueBillState string

const (
	requeueStateStaging   requeueBillState = "staging"
	requeueStateMatched   requeueBillState = "matched"
	requeueStateUnmatched requeueBillState = "unmatched"
)

// requeueBillRow 是一条内存账单行，字段与 SQL 里用到的列一一对应。
type requeueBillRow struct {
	id         int64
	state      requeueBillState
	occurredAt time.Time
	tokenName  string
	raw        map[string]any
	importedAt time.Time
}

// requeueBillStub 用内存状态模拟 reconciliation_upstream_bills 的状态迁移。
//
// 刻意逐条实现 SQL 谓词（match_state 条件 + occurred_at 半开区间 + LIMIT），
// 而不是简化成切片切割：本组测试要证明的正是「staging / matched 的账单不会被退回」，
// 简化掉谓词就等于把要测的东西测没了。
type requeueBillStub struct {
	ReconciliationUpstreamBillRepository

	rows []*requeueBillRow

	// requeueCalls 记录每次退回动作实际落库的 ID（按行顺序）。
	requeueCalls [][]int64
	// listLimits 记录每次取孤儿时的 LIMIT，用来断言上限口径。
	listLimits []int
	// refreshedAt 是「退回时把 imported_at 置为 now()」写入的时刻。
	refreshedAt []time.Time
}

func (s *requeueBillStub) ListUnmatched(_ context.Context, from, to time.Time, limit int) ([]ReconciliationUpstreamBill, error) {
	s.listLimits = append(s.listLimits, limit)

	out := make([]ReconciliationUpstreamBill, 0, len(s.rows))
	for _, row := range s.rows {
		if row.state != requeueStateUnmatched {
			continue
		}
		if row.occurredAt.Before(from) || !row.occurredAt.Before(to) {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, ReconciliationUpstreamBill{
			ID:         row.id,
			ImportedAt: row.importedAt,
			Payload: ReconciliationUpstreamBillPayload{
				OccurredAt: row.occurredAt,
				TokenName:  row.tokenName,
				Raw:        row.raw,
			},
		})
	}
	return out, nil
}

func (s *requeueBillStub) RequeueUnmatched(_ context.Context, billIDs []int64) (int64, error) {
	// 与 SQL 的 guard 一致：只有仍是 unmatched 的行才会被改写。
	target := make(map[int64]struct{}, len(billIDs))
	for _, billID := range billIDs {
		target[billID] = struct{}{}
	}

	now := time.Now().UTC()
	var affected int64
	applied := make([]int64, 0, len(billIDs))
	for _, row := range s.rows {
		if _, ok := target[row.id]; !ok {
			continue
		}
		if row.state != requeueStateUnmatched {
			continue
		}
		row.state = requeueStateStaging
		// 对应 SQL 里的 imported_at = now()：宽限期从这一刻重新起算。
		row.importedAt = now
		s.refreshedAt = append(s.refreshedAt, now)
		affected++
		applied = append(applied, row.id)
	}
	s.requeueCalls = append(s.requeueCalls, applied)
	return affected, nil
}

// ListStaging 供「退回后立刻匹配一轮」的取证：只取窗口内的 staging 行。
func (s *requeueBillStub) ListStaging(_ context.Context, from, to time.Time, limit int) ([]ReconciliationUpstreamBill, error) {
	out := make([]ReconciliationUpstreamBill, 0, len(s.rows))
	for _, row := range s.rows {
		if row.state != requeueStateStaging {
			continue
		}
		if row.occurredAt.Before(from) || !row.occurredAt.Before(to) {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, ReconciliationUpstreamBill{
			ID:         row.id,
			ImportedAt: row.importedAt,
			Payload: ReconciliationUpstreamBillPayload{
				OccurredAt: row.occurredAt,
				TokenName:  row.tokenName,
				Raw:        row.raw,
			},
		})
	}
	return out, nil
}

// FindDirectMatchCandidates / FindCompositeMatchCandidates 一律返回「没有候选调用」。
//
// 本组测试关心的是孤儿状态迁移，不是组合匹配的判定；返回空候选让 MatchStaging
// 走到孤儿判定那一步即可（嵌入接口而不实现会在运行时 panic，必须显式给出）。
func (s *requeueBillStub) FindDirectMatchCandidates(_ context.Context, _ string) ([]ReconciliationMatchCandidate, error) {
	return nil, nil
}

func (s *requeueBillStub) FindCompositeMatchCandidates(_ context.Context, _ ReconciliationCompositeQuery) ([]ReconciliationMatchCandidate, error) {
	return nil, nil
}

func (s *requeueBillStub) MarkUnmatched(_ context.Context, billIDs []int64) error {
	target := make(map[int64]struct{}, len(billIDs))
	for _, billID := range billIDs {
		target[billID] = struct{}{}
	}
	for _, row := range s.rows {
		if _, ok := target[row.id]; ok {
			row.state = requeueStateUnmatched
		}
	}
	return nil
}

func (s *requeueBillStub) row(id int64) *requeueBillRow {
	for _, row := range s.rows {
		if row.id == id {
			return row
		}
	}
	return nil
}

// newRequeueHarness 组装重试所需的依赖：规则集 + 内存账单表。
func newRequeueHarness(stagingLimit int, rows []*requeueBillRow, rules []ReconciliationAccountRule) (*ReconciliationSyncService, *requeueBillStub) {
	billRepo := &requeueBillStub{rows: rows}
	svc := NewReconciliationSyncService(
		&graceTestExtrasRepo{},
		billRepo,
		&externalKeyRuleRepo{rules: rules},
		newCollectCursorStateRepo(),
		nil, nil,
		ReconciliationSyncConfig{
			FxUSDCNYRate:     1,
			StagingLimit:     stagingLimit,
			MatchGracePeriod: 30 * time.Minute,
		},
	)
	return svc, billRepo
}

// renamedTokenRule 是修复后的规则形态：当前名字 + 改名不变的稳定 ID。
func renamedTokenRule() []ReconciliationAccountRule {
	return []ReconciliationAccountRule{{
		ID: 9, AccountID: 47, Provider: ReconciliationProviderA6,
		ExternalKey: "glm, id:41210", Enabled: true,
	}}
}

func TestRequeueUnmatched_OnlyRequeuesBillsCurrentRulesCanResolve(t *testing.T) {
	now := time.Now().UTC()
	occurredAt := now.Add(-time.Hour)
	window := func() (time.Time, time.Time) { return now.Add(-24 * time.Hour), now }

	rows := []*requeueBillRow{
		// 1：老名字 + 稳定 ID 命中规则 -> 应当复活
		{id: 1, state: requeueStateUnmatched, occurredAt: occurredAt, tokenName: "glm-3.5-95%",
			raw: map[string]any{"token_id": float64(41210)}, importedAt: now.Add(-72 * time.Hour)},
		// 2：规则里没有这个令牌 -> 不该复活（无差别退回会让它每轮重扫）
		{id: 2, state: requeueStateUnmatched, occurredAt: occurredAt, tokenName: "stranger",
			importedAt: now.Add(-72 * time.Hour)},
		// 3：命中规则的稳定 ID 但规则被停用 -> 不该复活
		{id: 3, state: requeueStateUnmatched, occurredAt: occurredAt, tokenName: "glm-3.5-95%",
			raw: map[string]any{"token_id": float64(41210)}, importedAt: now.Add(-72 * time.Hour)},
		// 4：当前名字直接命中 -> 应当复活
		{id: 4, state: requeueStateUnmatched, occurredAt: occurredAt, tokenName: "glm",
			importedAt: now.Add(-72 * time.Hour)},
		// 5：仍在 staging -> 不许被重复退回
		{id: 5, state: requeueStateStaging, occurredAt: occurredAt, tokenName: "glm",
			importedAt: now.Add(-time.Minute)},
		// 6：已经匹配成功 -> 绝不能被退回（退回会让对好的账重算）
		{id: 6, state: requeueStateMatched, occurredAt: occurredAt, tokenName: "glm",
			importedAt: now.Add(-72 * time.Hour)},
		// 7：窗口之外的孤儿 -> 本轮不处理
		{id: 7, state: requeueStateUnmatched, occurredAt: now.Add(-48 * time.Hour), tokenName: "glm",
			importedAt: now.Add(-72 * time.Hour)},
	}

	rules := []ReconciliationAccountRule{
		{ID: 9, AccountID: 47, Provider: ReconciliationProviderA6, ExternalKey: "glm, id:41210", Enabled: true},
		// 停用的规则：ID 命中也不算数（与匹配阶段同一套判定）。
		{ID: 10, AccountID: 48, Provider: ReconciliationProviderA6, ExternalKey: "id:41210", Enabled: false},
	}

	svc, billRepo := newRequeueHarness(100, rows, rules)
	from, to := window()

	requeued, err := svc.RequeueUnmatched(context.Background(), from, to)
	require.NoError(t, err)
	assert.EqualValues(t, 3, requeued, "只有规则能解析出账号的 3 条孤儿应当复活")

	require.Len(t, billRepo.requeueCalls, 1)
	assert.Equal(t, []int64{1, 3, 4}, billRepo.requeueCalls[0],
		"ID 通道（老名字 + token_id）与名字通道都要能复活；停用的规则不参与")

	assert.Equal(t, requeueStateStaging, billRepo.row(1).state)
	assert.Equal(t, requeueStateUnmatched, billRepo.row(2).state, "规则对不上的孤儿保持原状")
	assert.Equal(t, requeueStateUnmatched, billRepo.row(7).state, "窗口外的孤儿本轮不处理")

	assert.Equal(t, requeueStateStaging, billRepo.row(5).state, "staging 的账单不受影响")
	assert.Equal(t, requeueStateMatched, billRepo.row(6).state, "matched 的账单绝不能被退回")
}

func TestRequeueUnmatched_RefreshesGracePeriod(t *testing.T) {
	now := time.Now().UTC()
	occurredAt := now.Add(-time.Hour)

	rows := []*requeueBillRow{{
		id: 1, state: requeueStateUnmatched, occurredAt: occurredAt, tokenName: "glm-3.5-95%",
		raw:        map[string]any{"token_id": float64(41210)},
		importedAt: now.Add(-72 * time.Hour), // 三天前入库 -> 早已超出 30 分钟宽限期
	}}

	svc, billRepo := newRequeueHarness(100, rows, renamedTokenRule())

	requeued, err := svc.RequeueUnmatched(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.EqualValues(t, 1, requeued)

	// 退回后宽限期必须从「现在」重新起算：graceBase 优先读 imported_at，
	// 不刷新它的话下一轮匹配立刻又按「导入超过 30 分钟」把这条账单判回孤儿。
	row := billRepo.row(1)
	require.False(t, row.importedAt.IsZero(), "退回时必须写入新的入库时刻")
	assert.Less(t, time.Since(row.importedAt), 30*time.Minute, "刷新后的入库时刻应当在宽限期内")

	refreshed := ReconciliationUpstreamBill{ImportedAt: row.importedAt, Payload: ReconciliationUpstreamBillPayload{OccurredAt: occurredAt}}
	assert.Less(t, time.Since(graceBase(&refreshed)), 30*time.Minute,
		"刷新后的账单必须落在宽限期内，否则退回等于白做")

	// 反面对照：沿用原来的入库时刻时，同一套判定会立刻把它算成孤儿。
	stale := ReconciliationUpstreamBill{ImportedAt: now.Add(-72 * time.Hour), Payload: ReconciliationUpstreamBillPayload{OccurredAt: occurredAt}}
	assert.GreaterOrEqual(t, time.Since(graceBase(&stale)), 30*time.Minute,
		"未刷新的老账单会被立即判回孤儿——这正是必须改 imported_at 的原因")
}

// 退回之后立刻跑一轮匹配：宽限期已重置，匹配不上的账单必须留在 staging 等待，
// 而不是当场被判回孤儿（否则管理员点了重试，看板上的数字先掉再涨，来回抖动）。
func TestRequeueUnmatched_ThenMatchStagingKeepsBillsInGrace(t *testing.T) {
	now := time.Now().UTC()
	occurredAt := now.Add(-time.Hour)

	rows := []*requeueBillRow{{
		id: 1, state: requeueStateUnmatched, occurredAt: occurredAt, tokenName: "glm-3.5-95%",
		raw:        map[string]any{"token_id": float64(41210)},
		importedAt: now.Add(-72 * time.Hour),
	}}

	svc, billRepo := newRequeueHarness(100, rows, renamedTokenRule())
	from, to := now.Add(-24*time.Hour), now

	requeued, err := svc.RequeueUnmatched(context.Background(), from, to)
	require.NoError(t, err)
	require.EqualValues(t, 1, requeued)

	// 本桩不提供任何候选调用（FindCompositeMatchCandidates 返回空），
	// 因此这里只验证孤儿判定：账单必须留在 staging。
	matched, orphaned, err := svc.MatchStaging(context.Background(), from, to)
	require.NoError(t, err)
	assert.EqualValues(t, 0, matched)
	assert.EqualValues(t, 0, orphaned, "刚退回的账单在宽限期内，不能被判回孤儿")
	assert.Equal(t, requeueStateStaging, billRepo.row(1).state)
}

func TestRequeueUnmatched_NothingEligibleIsNoop(t *testing.T) {
	now := time.Now().UTC()

	rows := []*requeueBillRow{{
		id: 1, state: requeueStateUnmatched, occurredAt: now.Add(-time.Hour),
		tokenName: "stranger", importedAt: now.Add(-72 * time.Hour),
	}}

	svc, billRepo := newRequeueHarness(100, rows, renamedTokenRule())

	requeued, err := svc.RequeueUnmatched(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, requeued)
	assert.Empty(t, billRepo.requeueCalls, "没有合格账单时不应发出任何 UPDATE")
}

// 单批上限跟随 StagingLimit：退回的这批必须能被随后的一次匹配完整覆盖，
// 否则多退出来的账单会滞留在 staging（既不在孤儿计数里也没被匹配）。
func TestRequeueUnmatched_BatchLimitFollowsStagingLimit(t *testing.T) {
	now := time.Now().UTC()

	rows := make([]*requeueBillRow, 0, 3)
	for i := int64(1); i <= 3; i++ {
		rows = append(rows, &requeueBillRow{
			id: i, state: requeueStateUnmatched, occurredAt: now.Add(-time.Duration(i) * time.Minute),
			tokenName: "glm", importedAt: now.Add(-72 * time.Hour),
		})
	}

	svc, billRepo := newRequeueHarness(2, rows, renamedTokenRule())

	requeued, err := svc.RequeueUnmatched(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.EqualValues(t, 2, requeued, "StagingLimit=2 时单批最多退回 2 条")
	assert.Equal(t, []int{2}, billRepo.listLimits)
}

// 上限的缺省值：没有配置 StagingLimit 时退回 1000 条以内，绝不退化成「一次全捞」。
func TestRequeueUnmatched_DefaultBatchLimit(t *testing.T) {
	now := time.Now().UTC()

	rows := []*requeueBillRow{{
		id: 1, state: requeueStateUnmatched, occurredAt: now.Add(-time.Hour),
		tokenName: "glm", importedAt: now.Add(-72 * time.Hour),
	}}

	svc, billRepo := newRequeueHarness(0, rows, renamedTokenRule())

	_, err := svc.RequeueUnmatched(context.Background(), now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.Equal(t, []int{1000}, billRepo.listLimits)
}
