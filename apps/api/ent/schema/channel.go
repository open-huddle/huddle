package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Channel struct {
	ent.Schema
}

func (Channel) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (Channel) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		// slug is unique per organization, not globally — see Indexes below.
		// Allowed shape is enforced by handlers, not the schema, since the rule
		// (lowercase, hyphenated, ≤80 chars) may evolve.
		field.String("slug").NotEmpty(),
		field.String("topic").Optional(),
		field.String("description").Optional(),
	}
}

func (Channel) Edges() []ent.Edge {
	return []ent.Edge{
		// Required parent — every channel belongs to exactly one organization.
		edge.From("organization", Organization.Type).
			Ref("channels").
			Required().
			Unique(),
		// Informational only: the user who created the channel. Optional so a
		// future "delete user" flow does not orphan channels. No permissions
		// hang off this edge — channel creation/edit rights derive entirely
		// from organization membership role.
		edge.From("created_by", User.Type).
			Ref("created_channels").
			Unique(),
	}
}

func (Channel) Indexes() []ent.Index {
	return []ent.Index{
		// Per-organization slug uniqueness — keeps URLs like
		// /orgs/<org-slug>/channels/<channel-slug> conflict-free without
		// forcing global slug squatting across tenants.
		index.Edges("organization").Fields("slug").Unique(),
	}
}
