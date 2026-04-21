package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Membership struct {
	ent.Schema
}

func (Membership) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (Membership) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("role").Values("owner", "admin", "member").Default("member"),
	}
}

func (Membership) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).Ref("memberships").Required().Unique(),
		edge.From("organization", Organization.Type).Ref("memberships").Required().Unique(),
	}
}

func (Membership) Indexes() []ent.Index {
	return []ent.Index{
		index.Edges("user", "organization").Unique(),
	}
}
