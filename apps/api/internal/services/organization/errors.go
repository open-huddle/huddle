package organization

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation. Used to map slug collisions and duplicate memberships to
// CodeAlreadyExists rather than CodeInternal. SQLSTATE 23505 is the canonical
// code; relying on the string would tie us to message wording.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
