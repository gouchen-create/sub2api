package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// ReconciliationAccountRule 是某个本站账号与上游令牌的对应关系。
//
// 一个账号最多一条规则；version 每次保存自增，供调用侧快照比对。
//
// 刻意不对 (provider, external_key) 建唯一约束：同一个 A6 令牌允许被多个本站账号共用，
// 因此「令牌 -> 账号」是多对一关系，匹配时必须按候选集合处理。
type ReconciliationAccountRule struct {
	ent.Schema
}

func (ReconciliationAccountRule) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "reconciliation_account_rules"},
	}
}

func (ReconciliationAccountRule) Mixin() []ent.Mixin {
	return []ent.Mixin{mixins.TimeMixin{}}
}

func (ReconciliationAccountRule) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("account_id").
			Comment("本站账号 ID"),
		field.String("provider").
			MaxLen(16).
			Default("a6").
			Comment("上游提供方，目前只支持 a6"),
		field.String("external_key").
			MaxLen(128).
			Default("").
			Comment("上游令牌名（A6 的 token_name）"),
		field.Float("multiplier").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("预留字段：账号计费倍率。当前对账口径不使用"),
		field.Int64("version").
			Default(1).
			Comment("规则版本号，每次保存自增"),
		field.Bool("enabled").
			Default(true).
			Comment("是否启用该规则"),
	}
}

func (ReconciliationAccountRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id").Unique(),
		index.Fields("provider", "external_key"),
	}
}
