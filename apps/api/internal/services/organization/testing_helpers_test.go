package organization_test

import (
	"time"

	"github.com/open-huddle/huddle/apps/api/internal/services/organization"
)

// testInviteConfig returns a deterministic InviteConfig for tests. The
// secret only needs to be stable within one test run — the handler uses
// it to HMAC, and test assertions never look at the hash bytes directly.
// TTL is a full day so no test ever races against expiry unless it
// explicitly advances the Invitation row.
func testInviteConfig() organization.InviteConfig {
	return organization.InviteConfig{
		HMACSecret:  []byte("test-invites-secret-0123456789abcdef"),
		LinkBaseURL: "http://localhost:5173/accept-invite",
		TTL:         24 * time.Hour,
	}
}
