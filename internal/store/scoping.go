package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// EnsureUserScope creates the principal's user_scopes row with both
// defaults set to all, unless the row exists. An existing row is not
// changed. It is meant for test setup.
func EnsureUserScope(ctx context.Context, pool *pgxpool.Pool, principal string) error {
	err := db.New(pool).EnsureUserScope(ctx, db.EnsureUserScopeParams{
		Principal: principal, VerifiedDefault: "all", UnverifiedDefault: "all",
	})
	if err != nil {
		return fmt.Errorf("ensure user scope of %q: %w", principal, err)
	}

	return nil
}
