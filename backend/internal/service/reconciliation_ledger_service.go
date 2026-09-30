package service

import (
	"context"
	"errors"
	"time"
)

// 对账时间窗口的默认值与上下限。
const (
	// ReconciliationDefaultWindow 缺省窗口长度：当前时间往前 24 小时。
	ReconciliationDefaultWindow = 24 * time.Hour
	// ReconciliationMaxWindow 允许查询的最大窗口长度，防止一次拉取过重。
	ReconciliationMaxWindow = 366 * 24 * time.Hour
	// ReconciliationMaxPageSize 明细分页上限，与前端契约一致。
	ReconciliationMaxPageSize = 100
	// ReconciliationDefaultPageSize 明细分页缺省值，与前端契约一致。
	ReconciliationDefaultPageSize = 50
)

// 趋势分桶的候选粒度。窗口越长，桶越宽，避免点数量爆炸。
var (
	reconciliationBucketHour  = ReconciliationBucket{Label: "1小时", Duration: time.Hour}
	reconciliationBucket6Hour = ReconciliationBucket{Label: "6小时", Duration: 6 * time.Hour}
	reconciliationBucketDay   = ReconciliationBucket{Label: "1天", Duration: 24 * time.Hour}
	reconciliationBucketWeek  = ReconciliationBucket{Label: "1周", Duration: 7 * 24 * time.Hour}
)

// ReconciliationBucket 是一个趋势分桶粒度。
type ReconciliationBucket struct {
	Label    string
	Duration time.Duration
}

// 窗口解析错误。reason 用 UPPER_SNAKE，符合仓库错误约定。
var (
	// ErrReconciliationInvalidWindow 表示时间窗口无效（缺失、倒置或超出上限）。
	ErrReconciliationInvalidWindow = errors.New("RECONCILIATION_INVALID_WINDOW")
)

// ReconciliationTimeSeries 是趋势数据与所用的分桶粒度。
type ReconciliationTimeSeries struct {
	BucketLabel string
	Bucket      time.Duration
	Points      []ReconciliationBucketPoint
}

// ReconciliationLedgerService 负责看板所需的汇总、趋势与明细。
//
// 它不自带任何缓存：对账数据量小（按窗口聚合）、且必须实时反映管理员刚保存的规则，
// 缓存带来的复杂度大于收益。
type ReconciliationLedgerService struct {
	ledgerRepo ReconciliationLedgerRepository
}

// NewReconciliationLedgerService 创建对账账本服务。
func NewReconciliationLedgerService(ledgerRepo ReconciliationLedgerRepository) *ReconciliationLedgerService {
	return &ReconciliationLedgerService{ledgerRepo: ledgerRepo}
}

// ResolveWindow 把可选的入参规范化成半开区间 [from, to)。
//
// 缺省窗口是「当前时间往前 24 小时」；窗口倒置或超过上限时返回错误，
// 由接口层翻译成明确的 400，而不是静默改正成别的区间——静默改正会让用户
// 以为自己在看 A 区间，实际看到的是 B 区间。
func (s *ReconciliationLedgerService) ResolveWindow(from, to *time.Time) (time.Time, time.Time, error) {
	now := time.Now().UTC()

	resolvedTo := now
	if to != nil {
		resolvedTo = to.UTC()
	}
	resolvedFrom := resolvedTo.Add(-ReconciliationDefaultWindow)
	if from != nil {
		resolvedFrom = from.UTC()
	}

	if !resolvedFrom.Before(resolvedTo) {
		return time.Time{}, time.Time{}, ErrReconciliationInvalidWindow
	}
	if resolvedTo.Sub(resolvedFrom) > ReconciliationMaxWindow {
		return time.Time{}, time.Time{}, ErrReconciliationInvalidWindow
	}
	return resolvedFrom, resolvedTo, nil
}

// SelectBucket 按窗口长度选择趋势分桶粒度。
func SelectBucket(from, to time.Time) ReconciliationBucket {
	span := to.Sub(from)
	switch {
	case span <= 48*time.Hour:
		return reconciliationBucketHour
	case span <= 14*24*time.Hour:
		return reconciliationBucket6Hour
	case span <= 90*24*time.Hour:
		return reconciliationBucketDay
	default:
		return reconciliationBucketWeek
	}
}

// Summary 返回窗口内的汇总指标。
func (s *ReconciliationLedgerService) Summary(ctx context.Context, from, to time.Time) (*ReconciliationSummary, error) {
	return s.ledgerRepo.Summary(ctx, from, to)
}

// TimeSeries 返回窗口内的趋势数据。
func (s *ReconciliationLedgerService) TimeSeries(ctx context.Context, from, to time.Time) (*ReconciliationTimeSeries, error) {
	bucket := SelectBucket(from, to)
	points, err := s.ledgerRepo.Points(ctx, from, to, bucket.Duration)
	if err != nil {
		return nil, err
	}
	return &ReconciliationTimeSeries{
		BucketLabel: bucket.Label,
		Bucket:      bucket.Duration,
		Points:      points,
	}, nil
}

// Requests 返回明细页。
//
// status 只接受 all / matched / unmatched / upstream_unmatched，未知取值按 all 处理：
// 前端下拉框是唯一调用方，未知值意味着契约变更而非用户输入错误，退回全量比报错更有用。
func (s *ReconciliationLedgerService) Requests(ctx context.Context, from, to time.Time, status string, page, pageSize int) ([]ReconciliationLedgerRow, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = ReconciliationDefaultPageSize
	}
	if pageSize > ReconciliationMaxPageSize {
		pageSize = ReconciliationMaxPageSize
	}

	switch status {
	case "matched", "unmatched", "upstream_unmatched":
	default:
		status = "all"
	}

	return s.ledgerRepo.Rows(ctx, from, to, status, page, pageSize)
}

// UsageCountsByAccount 返回窗口内各账号的用量摘要，供规则页合并展示。
func (s *ReconciliationLedgerService) UsageCountsByAccount(ctx context.Context, from, to time.Time) (map[int64]ReconciliationAccountUsage, error) {
	return s.ledgerRepo.UsageCountsByAccount(ctx, from, to)
}
