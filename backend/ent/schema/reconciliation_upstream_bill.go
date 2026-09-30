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

// ReconciliationUpstreamBill 是一条上游（A6）逐笔账单。
//
// 成本与汇率在导入时冻结：一旦落库，cost_original / fx_rate_to_cny / cost_cny
// 不再随上游数据变动或汇率调整而更新（导入侧使用 ON CONFLICT DO NOTHING）。
//
// match_state 语义：
//   - staging   已导入、尚未完成首次匹配；此状态下不会在看板上显示为「上游待匹配」。
//   - matched   已匹配到本站某次调用，可与该调用合并成一行「已对账」。
//   - unmatched 确认匹配不上，作为孤儿账单在看板显示为「上游待匹配」。
//
// staging 这一中间态的存在意义：新导入的账单若立刻显示为「上游待匹配」，
// 看板上会短暂地同时出现下游调用行与上游账单行，看起来像重复记录。
//
// matched_usage_log_id 上有唯一部分索引，保证「一笔下游调用最多挂一笔上游账单」。
type ReconciliationUpstreamBill struct {
	ent.Schema
}

func (ReconciliationUpstreamBill) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "reconciliation_upstream_bills"},
	}
}

func (ReconciliationUpstreamBill) Mixin() []ent.Mixin {
	return []ent.Mixin{mixins.TimeMixin{}}
}

func (ReconciliationUpstreamBill) Fields() []ent.Field {
	return []ent.Field{
		field.String("provider").
			MaxLen(16).
			Default("a6").
			Comment("上游提供方，目前恒为 a6"),
		field.String("upstream_request_id").
			MaxLen(128).
			NotEmpty().
			Comment("上游账单声明的请求标识，用于直连匹配"),
		field.Time("occurred_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("上游账单发生时间"),
		field.Time("billing_date").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "date"}).
			Comment("上游账单归属日期，便于按日核对"),
		field.String("model").
			MaxLen(128).
			Default("").
			Comment("上游记录的模型名"),
		field.String("token_name").
			MaxLen(128).
			Default("").
			Comment("上游令牌名，用于关联本站账号"),
		field.Int("input_tokens").
			Default(0).
			Comment("输入 token 数，组合匹配依据"),
		field.Int("output_tokens").
			Default(0).
			Comment("输出 token 数，组合匹配依据"),
		field.Int("cache_read_tokens").
			Default(0).
			Comment("缓存读取 token 数，组合匹配依据"),
		field.Int("cache_creation_tokens").
			Default(0).
			Comment("缓存写入 token 数，组合匹配依据"),
		field.Int("cache_tokens_total").
			Default(0).
			Comment("上游口径的缓存合计；上游只回合并值时分列为 0，组合匹配以本列为准"),
		field.Float("cost_original").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("上游原币金额"),
		field.String("currency").
			MaxLen(8).
			Default("USD").
			Comment("上游结算币种"),
		field.Float("fx_rate_to_cny").
			Default(1).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("导入时的美元->人民币换算数字快照"),
		field.Float("cost_cny").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("换算后的上游成本，落库即冻结"),
		field.String("source").
			MaxLen(32).
			Default("a6").
			Comment("账单来源：a6 自动同步 / manual_import 人工导入"),
		field.String("match_state").
			MaxLen(16).
			Default("staging").
			Comment("匹配状态：staging / matched / unmatched"),
		field.String("match_method").
			MaxLen(48).
			Default("").
			Comment("匹配方式，记录是四级匹配中的哪一级命中的"),
		field.Int64("matched_usage_log_id").
			Optional().
			Nillable().
			Comment("匹配到的本站调用 ID（usage_logs.id）"),
		field.Int64("matched_account_id").
			Optional().
			Nillable().
			Comment("匹配到的本站账号 ID"),
		field.JSON("raw", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("上游原始账单留存，便于追溯与重放"),
		field.Time("imported_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("导入时间"),
	}
}

func (ReconciliationUpstreamBill) Indexes() []ent.Index {
	return []ent.Index{
		// 幂等导入：同一上游请求 ID 只落一条
		index.Fields("provider", "upstream_request_id").Unique(),
		// 「一笔下游调用最多挂一笔上游账单」的硬保证，不允许移除
		index.Fields("matched_usage_log_id").
			Unique().
			Annotations(entsql.IndexWhere("matched_usage_log_id IS NOT NULL")),
		index.Fields("occurred_at"),
		index.Fields("token_name", "occurred_at"),
		index.Fields("match_state"),
	}
}
