//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconciliationBillRepository_RequeueUnmatchedResetsGraceAnchor 锁死「退回重试」写下的 SQL 语义。
//
// 这条 SQL 有两个不能少的条件，少任何一个都会造成线上难查的错账：
//  1. imported_at = now()：孤儿宽限期由 graceBase() 决定，它优先读 imported_at。
//     老账单（几天前入库）只改 match_state 的话，下一轮匹配立刻又按「导入已超过宽限期」
//     把它们判回孤儿——退回动作等于白做，账单还会在「上游待匹配」计数里消失。
//  2. match_state = 'unmatched'：并发下若账单刚被匹配成功，必须放过它，
//     绝不能把已经对好的账退回队列。
//
// 这里不连数据库，只断言语句本身（同 account_repo_temp_unsched_test.go 的取证方式）：
// 列级行为完全由 SQL 文本决定，而 SQL 文本正是这段代码唯一的产出物。
func TestReconciliationBillRepository_RequeueUnmatchedResetsGraceAnchor(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(2)}
	repo := &reconciliationUpstreamBillRepository{sql: exec}

	affected, err := repo.RequeueUnmatched(context.Background(), []int64{101, 102})
	require.NoError(t, err)
	assert.EqualValues(t, 2, affected)

	require.Len(t, exec.execQueries, 1)
	query := normalizeSQLWhitespace(exec.execQueries[0])

	assert.Contains(t, query, "UPDATE reconciliation_upstream_bills")
	assert.Contains(t, query, "SET match_state = 'staging'")
	assert.Contains(t, query, "imported_at = now()",
		"必须重置宽限期起算点，否则退回的账单下一轮立刻又变孤儿")
	assert.Contains(t, query, "updated_at = now()")
	assert.Contains(t, query, "match_state = 'unmatched'",
		"guard 必须限定只改仍处于 unmatched 的账单，不能覆盖刚匹配成功的行")
	assert.Equal(t, []any{int64(101), int64(102)}, exec.execArgs[0], "占位符与账单 ID 必须一一对应")
}

// 空 ID 列表不产生任何语句：避免退化成一次无谓的更新往返。
func TestReconciliationBillRepository_RequeueUnmatchedEmptyIsNoop(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := &reconciliationUpstreamBillRepository{sql: exec}

	affected, err := repo.RequeueUnmatched(context.Background(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 0, affected)
	assert.Empty(t, exec.execQueries)
}

// reconciliationQueryRecorder 记录 SELECT/UPDATE 语句并让查询失败。
//
// 不需要真数据库：没有行可扫时 ListUnmatched 会原样返回错误，
// 而我们要取证的是它发出的那条 SQL（列清单与谓词）。
type reconciliationQueryRecorder struct {
	query string
	args  []any
}

func (r *reconciliationQueryRecorder) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, sql.ErrConnDone
}

func (r *reconciliationQueryRecorder) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	r.query = query
	r.args = append([]any(nil), args...)
	return nil, sql.ErrConnDone
}

// ListUnmatched 的取数条件：只取窗口内的孤儿，并且必须带上 raw 报文。
//
// raw 是规则写成 id:<数字> 时的唯一判定依据（token_id 没有独立列，只存在于报文里）；
// 漏掉这一列会让「按稳定 ID 复活」在最需要它的老账单上静默失效。
func TestReconciliationBillRepository_ListUnmatchedQueryShape(t *testing.T) {
	exec := &reconciliationQueryRecorder{}
	repo := &reconciliationUpstreamBillRepository{sql: exec}

	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

	_, err := repo.ListUnmatched(context.Background(), from, to, 500)
	require.Error(t, err, "查询失败必须原样向上传递，不能被吞掉")

	query := normalizeSQLWhitespace(exec.query)
	assert.Contains(t, query, "SELECT id, token_name, raw")
	assert.Contains(t, query, "match_state = 'unmatched'")
	assert.Contains(t, query, "occurred_at >= $1 AND occurred_at < $2")
	assert.Contains(t, query, "ORDER BY occurred_at, id", "取数顺序必须稳定，否则单批上限会漏账单")
	assert.Contains(t, query, "LIMIT $3")
	assert.Equal(t, []any{from, to, 500}, exec.args)
}
