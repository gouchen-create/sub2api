package repository

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/reconciliationaccountrule"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// reconciliationAccountRuleRepository 是账号规则（上游令牌 → 本站账号）的读写实现。
type reconciliationAccountRuleRepository struct {
	client *dbent.Client
}

// NewReconciliationAccountRuleRepository 创建账号规则仓库。
func NewReconciliationAccountRuleRepository(client *dbent.Client) service.ReconciliationAccountRuleRepository {
	return &reconciliationAccountRuleRepository{client: client}
}

// List 返回全部账号规则，按账号 ID 升序。
//
// 纯读语义：不做分页与过滤，规则页需要「全部账号」的完整视图来与账号列表对齐。
func (r *reconciliationAccountRuleRepository) List(ctx context.Context) ([]service.ReconciliationAccountRule, error) {
	client := clientFromContext(ctx, r.client)
	rows, err := client.ReconciliationAccountRule.Query().
		Order(dbent.Asc(reconciliationaccountrule.FieldAccountID)).
		All(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, nil, nil)
	}

	rules := make([]service.ReconciliationAccountRule, 0, len(rows))
	for _, row := range rows {
		rules = append(rules, toReconciliationAccountRule(row))
	}
	return rules, nil
}

// GetByAccountID 返回某个账号的规则；该账号没有规则时返回 (nil, nil)。
//
// 语义：没有规则是正常状态（「规则待配置」），不是错误 —— 用 NotFound 表达会逼着
// 调用方把正常分支写成错误分支；这里只把真正的持久层故障返回为 err。
func (r *reconciliationAccountRuleRepository) GetByAccountID(ctx context.Context, accountID int64) (*service.ReconciliationAccountRule, error) {
	client := clientFromContext(ctx, r.client)
	row, err := client.ReconciliationAccountRule.Query().
		Where(reconciliationaccountrule.AccountIDEQ(accountID)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, translatePersistenceError(err, nil, nil)
	}

	rule := toReconciliationAccountRule(row)
	return &rule, nil
}

// Upsert 按 account_id 写入规则，返回保存后的规则。
//
// 幂等语义（关键）：
//   - 新增：version = 1；
//   - 已存在：覆盖 provider / external_key / multiplier / enabled，且 version 必须 +1。
//
// version 是快照的归因依据：已落库的调用快照带着当时的 version，版本不变就等于
// 「规则从未改过」，历史数据会按新规则重新归因（线上曾因此误标 1768 条）。
//
// 返回值刻意回读数据库，而不是用 upsert 的 RETURNING 结果：ent 的 upsert 只回填
// 本次 INSERT 语句设置过的字段，自增后的 version 与 updated_at 只有从库里读回来才是真值。
func (r *reconciliationAccountRuleRepository) Upsert(
	ctx context.Context,
	accountID int64,
	provider, externalKey string,
	multiplier *float64,
	enabled bool,
) (*service.ReconciliationAccountRule, error) {
	client := clientFromContext(ctx, r.client)

	builder := client.ReconciliationAccountRule.Create().
		SetAccountID(accountID).
		SetProvider(provider).
		SetExternalKey(externalKey).
		SetVersion(1).
		SetEnabled(enabled)
	if multiplier != nil {
		builder.SetMultiplier(*multiplier)
	}

	if err := builder.
		OnConflictColumns(reconciliationaccountrule.FieldAccountID).
		Update(func(u *dbent.ReconciliationAccountRuleUpsert) {
			u.SetProvider(provider)
			u.SetExternalKey(externalKey)
			u.SetEnabled(enabled)
			u.AddVersion(1)
			if multiplier != nil {
				u.SetMultiplier(*multiplier)
			} else {
				// 乘数被清空时必须真的写回 NULL，否则旧乘数会继续生效。
				u.ClearMultiplier()
			}
		}).
		Exec(ctx); err != nil {
		return nil, translatePersistenceError(err, nil, nil)
	}

	return r.GetByAccountID(ctx, accountID)
}

// Delete 删除账号规则，返回实际删除的行数。
//
// 幂等语义：账号本来就没有规则时返回 (0, nil) —— 「删一个不存在的东西」结果是
// 「已经达成目标」，不是错误；调用方据此区分「删掉了」与「本来就没有」。
func (r *reconciliationAccountRuleRepository) Delete(ctx context.Context, accountID int64) (int64, error) {
	client := clientFromContext(ctx, r.client)
	affected, err := client.ReconciliationAccountRule.Delete().
		Where(reconciliationaccountrule.AccountIDEQ(accountID)).
		Exec(ctx)
	if err != nil {
		return 0, translatePersistenceError(err, nil, nil)
	}
	return int64(affected), nil
}

// toReconciliationAccountRule 把 ent 实体转换为服务层规则模型。
func toReconciliationAccountRule(e *dbent.ReconciliationAccountRule) service.ReconciliationAccountRule {
	return service.ReconciliationAccountRule{
		ID:          e.ID,
		AccountID:   e.AccountID,
		Provider:    e.Provider,
		ExternalKey: e.ExternalKey,
		Multiplier:  e.Multiplier,
		Version:     e.Version,
		Enabled:     e.Enabled,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
	}
}
