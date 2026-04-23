package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type User struct {
	ent.Schema
}

func (User) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (User) Fields() []ent.Field {
	return []ent.Field{
		// Canonical identity: the OIDC `sub` claim from whichever IDP issued it.
		// Stable across email changes, so it — not email — is the unique key.
		field.String("subject").NotEmpty().Unique(),
		field.String("email").NotEmpty(),
		field.String("display_name").Optional(),
	}
}

func (User) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email"),
	}
}

func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("memberships", Membership.Type),
		// Channels this user created. Informational; not used for authz.
		edge.To("created_channels", Channel.Type),
		// Messages authored by this user. Required-from-Message side, so a
		// User with messages cannot be deleted without first scrubbing them.
		edge.To("messages", Message.Type),
		// Invitations this user issued as an admin/owner.
		edge.To("invitations_sent", Invitation.Type),
		// Invitations this user consumed on signup. Optional on the
		// Invitation side — rows remain with NULL until accept.
		edge.To("invitations_accepted", Invitation.Type),
	}
}
