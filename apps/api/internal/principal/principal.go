// Package principal turns OIDC claims into the canonical *ent.User row,
// upserting on first sight. It exists so handlers and the policy engine never
// need to repeat the "look up the calling user by subject" dance — and so the
// upsert side-effect lives in one place rather than at every entry point.
package principal

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/user"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
)

// Resolver maps an authenticated request's claims to a database row. The
// returned *ent.User is always materialized — first-call upsert is the
// resolver's responsibility, not the handler's.
type Resolver struct {
	client *ent.Client
}

func NewResolver(client *ent.Client) *Resolver {
	return &Resolver{client: client}
}

// Resolve returns the *ent.User for the principal in ctx. The first call for
// a given subject creates the row; subsequent calls refresh email and
// display_name from the latest token claims, so changes upstream propagate
// without operator intervention.
func (r *Resolver) Resolve(ctx context.Context) (*ent.User, error) {
	c, ok := auth.ClaimsFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("principal: %w", auth.ErrNoClaims)
	}
	id, err := r.client.User.Create().
		SetSubject(c.Subject).
		SetEmail(c.Email).
		SetDisplayName(c.DisplayName()).
		OnConflictColumns(user.FieldSubject).
		Update(func(u *ent.UserUpsert) {
			u.SetEmail(c.Email)
			u.SetDisplayName(c.DisplayName())
			u.UpdateUpdatedAt()
		}).
		ID(ctx)
	if err != nil {
		return nil, fmt.Errorf("principal: upsert: %w", err)
	}
	u, err := r.client.User.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("principal: get: %w", err)
	}
	return u, nil
}

// ErrNoClaims is re-exported for callers that don't already import auth.
var ErrNoClaims = errors.New("principal: no authenticated principal")
