//go:build integration

package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// The reconciler's pass deletes stale rows, then runs the wrapped pass even
// when that pass fails.
func TestRowCleanupRunsOnThePass(t *testing.T) {
	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	_, err = pool.Exec(t.Context(), `INSERT INTO ui_logins (state_hash, nonce, pkce_verifier, created_at)
		VALUES (sha256('stale'::bytea), 'n', 'v', now() - interval '11 minutes')`)
	require.NoError(t, err)

	failed := errors.New("pass failed")
	ran := false
	pass := runtime.RowCleanup(db.New(pool), func(context.Context) error {
		ran = true

		return failed
	})

	require.ErrorIs(t, pass(t.Context()), failed)
	assert.True(t, ran, "the wrapped pass runs")

	var left int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM ui_logins").Scan(&left))
	assert.Zero(t, left, "the stale sign-in is deleted")
}
