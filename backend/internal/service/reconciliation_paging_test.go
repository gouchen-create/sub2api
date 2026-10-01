//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 明细页参数归一化 ====================
//
// 导出这两个函数的唯一理由是「生效值回显」：接口层必须用同一份实现算出
// 真正生效的 page_size / status 并原样回显。旧代码里接口层「越界退回 50」、
// 服务层「越界截到 100」，于是 ?page_size=200 会按 100 取数、回显 50、
// total_pages 也按 50 算，前端分页控件与真实数据集对不上。

func TestNormalizeReconciliationPageSize(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  int
	}{
		{name: "缺省 0 取默认值", input: 0, want: ReconciliationDefaultPageSize},
		{name: "负数取默认值", input: -20, want: ReconciliationDefaultPageSize},
		{name: "1 是合法下限", input: 1, want: 1},
		{name: "默认值原样保留", input: 50, want: 50},
		{name: "上限原样保留", input: 100, want: 100},
		{name: "超过上限截到上限", input: 200, want: ReconciliationMaxPageSize},
		{name: "远超上限同样截到上限", input: 100000, want: ReconciliationMaxPageSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeReconciliationPageSize(tc.input))
		})
	}
}

func TestNormalizeReconciliationStatus(t *testing.T) {
	for _, status := range []string{"matched", "unmatched", "upstream_unmatched"} {
		assert.Equal(t, status, NormalizeReconciliationStatus(status), "受支持的取值必须原样保留")
	}
	for _, status := range []string{"all", "", "bogus", "MATCHED", " pending"} {
		assert.Equal(t, "all", NormalizeReconciliationStatus(status),
			"未知取值一律按 all 处理（%q）", status)
	}
}

// TestRequestsUsesNormalizedValues 服务层自己也要归一化：
// 服务是公开入口，不能假设调用方已经归一化过。
func TestRequestsUsesNormalizedValues(t *testing.T) {
	repo := &ledgerNormalizeStub{}
	svc := NewReconciliationLedgerService(repo)

	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)

	_, _, err := svc.Requests(context.Background(), from, to, "bogus", 0, 250)
	require.NoError(t, err)
	assert.Equal(t, "all", repo.status, "未知 status 必须按 all 下发")
	assert.Equal(t, 1, repo.page, "page < 1 必须收敛到 1")
	assert.Equal(t, ReconciliationMaxPageSize, repo.pageSize, "page_size 超限必须收敛到上限")
}

// ledgerNormalizeStub 记录服务层透传给仓库的参数。
type ledgerNormalizeStub struct {
	ReconciliationLedgerRepository

	status   string
	page     int
	pageSize int
}

func (s *ledgerNormalizeStub) Rows(_ context.Context, _, _ time.Time, status string, page, pageSize int) ([]ReconciliationLedgerRow, int64, error) {
	s.status, s.page, s.pageSize = status, page, pageSize
	return nil, 0, nil
}
