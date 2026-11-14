//go:build integration

package gateway_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceKeysUpgrade(t *testing.T) {
	t.Run("change approval rolls back a key request", func(t *testing.T) {
		fixture := newMCPFixture(t)
		api := newWorkspaceAPI(t, fixture, true)
		id := "0123456789abcdef0123456789abcdef"

		_, err := fixture.pool.Exec(t.Context(), `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
			VALUES ($1, 'alice', 'key', $2, true,
			        jsonb_build_object('name', 'dev', 'username', 'alice', 'keyID', repeat('a', 43), 'publicKey', repeat('A', 43)),
			        now() + interval '1 hour')`, id, aliceWorkspace)
		require.NoError(t, err)

		err = pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			return api.workspaces.ApproveChangeRequest(t.Context(), tx, aliceIdentity(), id)
		})
		require.Error(t, err)

		var status string

		require.NoError(t, fixture.pool.QueryRow(t.Context(), `SELECT status FROM access_requests WHERE id = $1`, id).Scan(&status))
		assert.Equal(t, "pending", status)
	})
}
