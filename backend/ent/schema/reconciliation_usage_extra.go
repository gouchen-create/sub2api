package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// ReconciliationUsageExtra 是「经营对账」中调用侧的扩展快照。
//
// 它只承载两样主库没有、且必须被冻结的东西：
//  1. 调用发生时该账号归属哪个上游令牌 —— 规则之后会被管理员修改，历史不能重算
//  2. 采集时的美元->人民币换算数字 —— 汇率之后会变，历史收入不能重算
//
// 下游收入原值仍保存在 usage_logs.actual_cost，本表刻意不复制它，避免双写不一致。
// usage_log_id 不加外键约束，避免影响 usage_logs 的高频写入路径。
type ReconciliationUsageExtra struct {
	ent.Schema
}

func (ReconciliationUsageExtra) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "reconciliation_usage_extras"},
	}
}

func (ReconciliationUsageExtra) Mixin() []ent.Mixin {
	return []ent.Mixin{mixins.TimeMixin{}}
}

func (ReconciliationUsageExtra) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("usage_log_id").
			Comment("对应 usage_logs.id"),
		field.Int64("account_id").
			Comment("调用使用的账号 ID，冗余用于按账号聚合"),
		field.String("rule_provider").
			MaxLen(16).
			Default("").
			Comment("采集时的规则来源，空串表示当时该账号没有规则"),
		field.String("rule_external_key").
			MaxLen(128).
			Default("").
			Comment("采集时的上游令牌名，用于令牌改名后回查历史账单"),
		field.Int64("rule_version").
			Default(0).
			Comment("采集时的规则版本号"),
		field.Float("revenue_original").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("下游收入原值（usage_logs.actual_cost 的快照）"),
		field.Float("fx_rate_to_cny").
			Default(1).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("采集时的美元->人民币换算数字快照"),
		field.Float("revenue_cny").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("换算后的下游收入，落库即冻结"),
		field.Time("collected_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("采集时间"),
	}
}

func (ReconciliationUsageExtra) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("usage_log_id").Unique(),
		index.Fields("collected_at"),
		index.Fields("account_id", "collected_at"),
		index.Fields("rule_external_key"),
	}
}
