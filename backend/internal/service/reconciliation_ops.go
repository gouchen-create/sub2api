package service

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/shopspring/decimal"
)

// ReconciliationStateKeyFxRateOverride 是汇率的运行时覆盖值。
//
// 汇率默认来自配置项，但允许管理员在不重启、不重新部署的情况下改数：
// 面板上没有输入框（前端要保持零改动），所以覆盖值写在同步状态表里，
// 改一行即可生效，同时汇总接口会把它显示在页面上。
const ReconciliationStateKeyFxRateOverride = "fx_usd_cny_rate_override"

// EffectiveFxRate 返回当前生效的换算数字：优先取运行时覆盖值，其次取配置默认值。
func (s *ReconciliationSyncService) EffectiveFxRate(ctx context.Context) float64 {
	rate, _ := resolveReconciliationFxRate(ctx, s.stateRepo, s.cfg.FxUSDCNYRate)
	return rate
}

// SetFxRateOverride 设置汇率覆盖值；传 0 或负数表示清除覆盖、回到配置默认值。
func (s *ReconciliationSyncService) SetFxRateOverride(ctx context.Context, rate float64) error {
	return setReconciliationFxRateOverride(ctx, s.stateRepo, rate)
}

// ==================== 汇率覆盖的唯一实现 ====================
//
// 下面几个函数是「面板覆盖 → config.reconciliation.fx_usd_cny_rate → 1」这条规则的
// 唯一实现：同步服务（记账时用）与 A6 设置服务（页面展示与保存时用）都走它们。
// 两边各写一份副本的话，改了一处忘了另一处，页面上显示的汇率就会和实际记账用的
// 汇率对不上——这正是「统一、可测」要求防的事。

// normalizeReconciliationFxDefault 归一化配置里的汇率默认值：<= 0（没配或写坏）按 1 处理。
func normalizeReconciliationFxDefault(rate float64) float64 {
	if rate <= 0 {
		return 1
	}
	return rate
}

// resolveReconciliationFxRate 解析生效汇率，并报告它是否来自面板覆盖。
func resolveReconciliationFxRate(ctx context.Context, stateRepo ReconciliationSyncStateRepository, fallback float64) (float64, bool) {
	fallback = normalizeReconciliationFxDefault(fallback)
	if stateRepo == nil {
		return fallback, false
	}
	values, err := stateRepo.GetMultiple(ctx, []string{ReconciliationStateKeyFxRateOverride})
	if err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "fx_override_read_failed: err=%v", err)
		return fallback, false
	}
	parsed, ok := parseReconciliationFxRate(values[ReconciliationStateKeyFxRateOverride])
	if !ok {
		return fallback, false
	}
	return parsed, true
}

// parseReconciliationFxRate 解析面板覆盖的汇率字面量。
//
// ok=false 表示这个值不可用（空白、非数字、NaN/Inf、非正数），调用方退回配置默认值：
// 一个写坏的覆盖值绝不能把全部金额算成 0。
func parseReconciliationFxRate(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		logger.LegacyPrintf("service.reconciliation_sync", "fx_override_invalid: raw=%q", raw)
		return 0, false
	}
	return parsed, true
}

// setReconciliationFxRateOverride 写入汇率覆盖值；<= 0 表示清除覆盖。
func setReconciliationFxRateOverride(ctx context.Context, stateRepo ReconciliationSyncStateRepository, rate float64) error {
	if rate <= 0 {
		return stateRepo.Set(ctx, ReconciliationStateKeyFxRateOverride, "")
	}
	return stateRepo.Set(ctx, ReconciliationStateKeyFxRateOverride, strconv.FormatFloat(rate, 'f', -1, 64))
}

// ReconciliationBackfillStatus 是历史回填的进度快照。
type ReconciliationBackfillStatus struct {
	// Status 取值：空（从未回填）、running、done、failed。
	Status    string
	Running   bool
	From      *time.Time
	To        *time.Time
	Cursor    *time.Time
	Processed int64
	Error     string
}

// reconciliationBackfillChunk 是回填时每段推进的时间跨度。
//
// 分段推进而不是一次性拉完整个区间：上游分页有上限，且中途失败时可以
// 从游标继续，不必从头再来。
const reconciliationBackfillChunk = 6 * time.Hour

// backfillMu 保证同一进程内只有一次回填在跑。
var backfillMu sync.Mutex

// StartBackfill 启动历史账单回填，立即返回本次回填覆盖的窗口。
//
// 缺省窗口为「配置回看长度 往前 30 天」。回填在后台跑，进度通过
// BackfillStatus 查询，接口层不会因为上游慢而超时。
func (s *ReconciliationSyncService) StartBackfill(ctx context.Context, from, to *time.Time) (time.Time, time.Time, error) {
	now := time.Now().UTC()

	resolvedTo := now
	if to != nil {
		resolvedTo = to.UTC()
	}
	resolvedFrom := resolvedTo.Add(-30 * 24 * time.Hour)
	if from != nil {
		resolvedFrom = from.UTC()
	}
	if !resolvedFrom.Before(resolvedTo) {
		return time.Time{}, time.Time{}, ErrReconciliationInvalidWindow
	}
	if s.billSource == nil {
		return time.Time{}, time.Time{}, ErrReconciliationBillSourceUnavailable
	}

	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillStatus, ReconciliationBackfillStatusRunning); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillFrom, resolvedFrom.Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillTo, resolvedTo.Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillCursor, resolvedFrom.Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillProcessed, "0"); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillError, ""); err != nil {
		return time.Time{}, time.Time{}, err
	}

	runCtx := context.WithoutCancel(ctx)
	go s.runBackfill(runCtx, resolvedFrom, resolvedTo)

	return resolvedFrom, resolvedTo, nil
}

// runBackfill 在后台逐段拉取历史账单并顺带匹配。
func (s *ReconciliationSyncService) runBackfill(ctx context.Context, from, to time.Time) {
	if !backfillMu.TryLock() {
		// 已有回填在跑：把状态改回 running 之外会误导，保持原状即可。
		logger.LegacyPrintf("service.reconciliation_sync", "backfill_already_running: skipped=true")
		return
	}
	defer backfillMu.Unlock()

	var processed int64
	cursor := from
	for cursor.Before(to) {
		segmentEnd := cursor.Add(reconciliationBackfillChunk)
		if segmentEnd.After(to) {
			segmentEnd = to
		}

		inserted, err := s.SyncA6Bills(ctx, cursor, segmentEnd)
		if err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "backfill_segment_failed: from=%s to=%s err=%v",
				cursor.Format(time.RFC3339), segmentEnd.Format(time.RFC3339), err)
			s.finishBackfill(ctx, ReconciliationBackfillStatusFailed, processed, err.Error())
			return
		}
		processed += inserted

		if _, _, err := s.MatchStaging(ctx, cursor, segmentEnd); err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "backfill_match_failed: from=%s to=%s err=%v",
				cursor.Format(time.RFC3339), segmentEnd.Format(time.RFC3339), err)
		}

		cursor = segmentEnd
		if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillCursor, cursor.Format(time.RFC3339Nano)); err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "backfill_cursor_write_failed: err=%v", err)
		}
		if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillProcessed, strconv.FormatInt(processed, 10)); err != nil {
			logger.LegacyPrintf("service.reconciliation_sync", "backfill_processed_write_failed: err=%v", err)
		}
	}

	s.finishBackfill(ctx, ReconciliationBackfillStatusDone, processed, "")
}

func (s *ReconciliationSyncService) finishBackfill(ctx context.Context, status string, processed int64, failure string) {
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillStatus, status); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "backfill_status_write_failed: err=%v", err)
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillProcessed, strconv.FormatInt(processed, 10)); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "backfill_processed_write_failed: err=%v", err)
	}
	if err := s.stateRepo.Set(ctx, ReconciliationStateKeyBackfillError, failure); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "backfill_error_write_failed: err=%v", err)
	}
}

// BackfillStatus 读取回填进度。从未回填过时 Status 为空字符串。
func (s *ReconciliationSyncService) BackfillStatus(ctx context.Context) (*ReconciliationBackfillStatus, error) {
	keys := []string{
		ReconciliationStateKeyBackfillStatus,
		ReconciliationStateKeyBackfillFrom,
		ReconciliationStateKeyBackfillTo,
		ReconciliationStateKeyBackfillCursor,
		ReconciliationStateKeyBackfillProcessed,
		ReconciliationStateKeyBackfillError,
	}
	values, err := s.stateRepo.GetMultiple(ctx, keys)
	if err != nil {
		return nil, err
	}

	status := &ReconciliationBackfillStatus{
		Status:  values[ReconciliationStateKeyBackfillStatus],
		Error:   values[ReconciliationStateKeyBackfillError],
		Running: values[ReconciliationStateKeyBackfillStatus] == ReconciliationBackfillStatusRunning,
	}
	status.From = parseReconciliationTime(values[ReconciliationStateKeyBackfillFrom])
	status.To = parseReconciliationTime(values[ReconciliationStateKeyBackfillTo])
	status.Cursor = parseReconciliationTime(values[ReconciliationStateKeyBackfillCursor])
	if raw := strings.TrimSpace(values[ReconciliationStateKeyBackfillProcessed]); raw != "" {
		if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			status.Processed = parsed
		}
	}
	return status, nil
}

func parseReconciliationTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &parsed
}

// ReconciliationUpstreamRecordInput 是手动导入一条上游账单的输入。
//
// 字段与前端契约一一对应；除上游请求 ID 外全部可选，缺失时按 A6 的默认口径补齐。
type ReconciliationUpstreamRecordInput struct {
	Provider          string
	UpstreamRequestID string
	Cost              float64
	Currency          string
	FxRateToCNY       float64
	OccurredAt        *time.Time
	Model             string
	TokenName         string
	InputTokens       int
	OutputTokens      int
	CacheReadTokens   int
	CacheCreation     int
	CacheTokensTotal  int
	Source            string
}

// ImportUpstreamRecords 手动导入上游账单并返回新增条数。
//
// 与自动拉取走同一条写入路径，因此同样享受幂等保护：已存在的账单不会被覆盖，
// 汇率与金额在首次导入时冻结。
func (s *ReconciliationSyncService) ImportUpstreamRecords(ctx context.Context, records []ReconciliationUpstreamRecordInput) (int64, error) {
	if len(records) == 0 {
		return 0, nil
	}

	fxRate := s.EffectiveFxRate(ctx)

	payloads := make([]ReconciliationUpstreamBillPayload, 0, len(records))
	now := time.Now().UTC()
	for i := range records {
		record := &records[i]
		occurredAt := now
		if record.OccurredAt != nil {
			occurredAt = record.OccurredAt.UTC()
		}
		rate := record.FxRateToCNY
		if rate <= 0 {
			rate = fxRate
		}
		provider := strings.TrimSpace(record.Provider)
		if provider == "" {
			provider = ReconciliationProviderA6
		}
		source := strings.TrimSpace(record.Source)
		if source == "" {
			source = "manual"
		}
		currency := strings.TrimSpace(record.Currency)
		if currency == "" {
			currency = "USD"
		}
		cacheTotal := record.CacheTokensTotal
		if cacheTotal <= 0 {
			cacheTotal = record.CacheReadTokens + record.CacheCreation
		}

		payloads = append(payloads, ReconciliationUpstreamBillPayload{
			Provider:            provider,
			UpstreamRequestID:   strings.TrimSpace(record.UpstreamRequestID),
			OccurredAt:          occurredAt,
			Model:               record.Model,
			TokenName:           record.TokenName,
			InputTokens:         record.InputTokens,
			OutputTokens:        record.OutputTokens,
			CacheReadTokens:     record.CacheReadTokens,
			CacheCreationTokens: record.CacheCreation,
			CacheTokensTotal:    cacheTotal,
			CostOriginal:        record.Cost,
			Currency:            currency,
			FxRateToCNY:         rate,
			CostCNY:             decimal.NewFromFloat(record.Cost).Mul(decimal.NewFromFloat(rate)).InexactFloat64(),
			Source:              source,
		})
	}

	inserted, err := s.billRepo.UpsertBatch(ctx, payloads)
	if err != nil {
		return inserted, err
	}
	if _, _, err := s.MatchStaging(ctx, now.Add(-30*24*time.Hour), now.Add(time.Minute)); err != nil {
		logger.LegacyPrintf("service.reconciliation_sync", "manual_import_match_failed: err=%v", err)
	}
	return inserted, nil
}
