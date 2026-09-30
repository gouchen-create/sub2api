package repository

import (
	"context"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/reconciliationusageextra"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// reconciliationUsageExtraRepository 是调用侧快照（收入与汇率）的读写实现。
type reconciliationUsageExtraRepository struct {
	client *dbent.Client
}

// NewReconciliationUsageExtraRepository 创建调用侧快照仓库。
func NewReconciliationUsageExtraRepository(client *dbent.Client) service.ReconciliationUsageExtraRepository {
	return &reconciliationUsageExtraRepository{client: client}
}

// UpsertBatch 幂等写入调用侧快照，返回本批实际新增的条数。
//
// 幂等语义：usage_log_id 上有唯一索引，冲突时 ON CONFLICT DO NOTHING ——
// 快照一旦落库即冻结，重放采集不得覆盖收入、汇率等已落库金额
// （汇率会随时间漂移，覆盖等于改写历史账）。
//
// 「实际新增」= 本批按 usage_log_id 去重后的条数 − 插入前已存在的条数。
// 计数只用于采集日志与指标；即使并发采集同一批，DO NOTHING 也保证不会覆盖旧行。
func (r *reconciliationUsageExtraRepository) UpsertBatch(ctx context.Context, extras []service.ReconciliationUsageExtra) (int64, error) {
	// 同批去重：同一条调用重复出现在批里既无意义，也会让新增条数虚高。
	deduped := make([]service.ReconciliationUsageExtra, 0, len(extras))
	usageLogIDs := make([]int64, 0, len(extras))
	seen := make(map[int64]struct{}, len(extras))
	for _, extra := range extras {
		// usage_log_id 是快照的主键语义，缺了它无法去重、也无法判重，直接跳过。
		if extra.UsageLogID <= 0 {
			continue
		}
		if _, exists := seen[extra.UsageLogID]; exists {
			continue
		}
		seen[extra.UsageLogID] = struct{}{}
		deduped = append(deduped, extra)
		usageLogIDs = append(usageLogIDs, extra.UsageLogID)
	}
	if len(deduped) == 0 {
		return 0, nil
	}

	client := clientFromContext(ctx, r.client)

	existing, err := r.listCollectedUsageLogIDs(ctx, client, usageLogIDs)
	if err != nil {
		return 0, translatePersistenceError(err, nil, nil)
	}

	builders := make([]*dbent.ReconciliationUsageExtraCreate, 0, len(deduped))
	for _, extra := range deduped {
		builder := client.ReconciliationUsageExtra.Create().
			SetUsageLogID(extra.UsageLogID).
			SetAccountID(extra.AccountID).
			SetRuleProvider(extra.RuleProvider).
			SetRuleExternalKey(extra.RuleExternalKey).
			SetRuleVersion(extra.RuleVersion).
			SetRevenueOriginal(extra.RevenueOriginal).
			SetFxRateToCny(extra.FxRateToCNY).
			SetRevenueCny(extra.RevenueCNY)
		// 采集时间由调用方冻结；未提供时交给 ent 的默认值（入库时刻）。
		if !extra.CollectedAt.IsZero() {
			builder.SetCollectedAt(extra.CollectedAt)
		}
		builders = append(builders, builder)
	}

	if err := client.ReconciliationUsageExtra.CreateBulk(builders...).
		OnConflictColumns(reconciliationusageextra.FieldUsageLogID).
		DoNothing().
		Exec(ctx); err != nil {
		return 0, translatePersistenceError(err, nil, nil)
	}

	return int64(len(deduped) - len(existing)), nil
}

// ListCollectedUsageLogIDs 返回给定 usage_log_id 中已经采集过的部分。
//
// 语义：纯读探测，不产生任何写入；采集侧据此跳过重复采集。
// 未出现在返回值里的 ID 即「还没采过」，因此调用方必须按
// `_, ok := collected[id]` 判断，不能把缺失当成 false 值以外的含义。
func (r *reconciliationUsageExtraRepository) ListCollectedUsageLogIDs(ctx context.Context, usageLogIDs []int64) (map[int64]struct{}, error) {
	if len(usageLogIDs) == 0 {
		return map[int64]struct{}{}, nil
	}

	client := clientFromContext(ctx, r.client)
	collected, err := r.listCollectedUsageLogIDs(ctx, client, usageLogIDs)
	if err != nil {
		return nil, translatePersistenceError(err, nil, nil)
	}
	return collected, nil
}

// listCollectedUsageLogIDs 是存在性探测的共用实现，只取 usage_log_id 一列。
func (r *reconciliationUsageExtraRepository) listCollectedUsageLogIDs(ctx context.Context, client *dbent.Client, usageLogIDs []int64) (map[int64]struct{}, error) {
	if len(usageLogIDs) == 0 {
		return map[int64]struct{}{}, nil
	}

	rows, err := client.ReconciliationUsageExtra.Query().
		Where(reconciliationusageextra.UsageLogIDIn(usageLogIDs...)).
		Select(reconciliationusageextra.FieldUsageLogID).
		All(ctx)
	if err != nil {
		return nil, err
	}

	collected := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		collected[row.UsageLogID] = struct{}{}
	}
	return collected, nil
}

// ListAccountIDsByRuleKeys 按令牌名（rule_external_key）反查历史上用过这些令牌名的账号，去重返回。
//
// 用途：上游令牌改名后，历史账单仍要能落回原账号，所以这里只查历史快照、不做时间过滤。
// 幂等/纯读语义：不写任何数据，重复调用结果一致（按账号 ID 升序，便于比对）。
//
// 空串不参与反查：快照里 rule_external_key 的零值就是空串，放进去会把所有
// 「没记令牌名」的账号一并拉出来，让组合匹配的候选集无意义地膨胀。
func (r *reconciliationUsageExtraRepository) ListAccountIDsByRuleKeys(ctx context.Context, ruleKeys []string) ([]int64, error) {
	keys := make([]string, 0, len(ruleKeys))
	seen := make(map[string]struct{}, len(ruleKeys))
	for _, key := range ruleKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return []int64{}, nil
	}

	client := clientFromContext(ctx, r.client)

	// 去重交给数据库：历史快照可能有上百万行，不能在 Go 侧拉全量再去重。
	var accountIDs []int64
	if err := client.ReconciliationUsageExtra.Query().
		Where(reconciliationusageextra.RuleExternalKeyIn(keys...)).
		Order(dbent.Asc(reconciliationusageextra.FieldAccountID)).
		GroupBy(reconciliationusageextra.FieldAccountID).
		Scan(ctx, &accountIDs); err != nil {
		return nil, translatePersistenceError(err, nil, nil)
	}
	if accountIDs == nil {
		return []int64{}, nil
	}
	return accountIDs, nil
}
