package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// APIKeyRouteFailoverTarget stores an API key's ordered fallback group targets.
type APIKeyRouteFailoverTarget struct {
	ent.Schema
}

func (APIKeyRouteFailoverTarget) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "api_key_route_failover_targets"},
	}
}

func (APIKeyRouteFailoverTarget) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (APIKeyRouteFailoverTarget) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("api_key_id"),
		field.Int64("target_group_id"),
		field.Int("priority").Min(1).Max(5),
	}
}

func (APIKeyRouteFailoverTarget) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("api_key", APIKey.Type).
			Ref("route_failover_targets").
			Field("api_key_id").
			Unique().
			Required(),
		edge.To("target_group", Group.Type).
			Field("target_group_id").
			Unique().
			Required().
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

func (APIKeyRouteFailoverTarget) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("api_key_id", "target_group_id").Unique(),
		index.Fields("api_key_id", "priority").Unique(),
		index.Fields("target_group_id"),
	}
}
