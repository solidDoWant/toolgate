//go:build integration

package secrets_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestBrokerKeepsEachUsersSecretsApartInPostgreSQL(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	broker := secrets.NewBroker(db.New(pool), newCipher(t, newKeyset(t)))

	_, err = broker.Set(t.Context(), "alice", "github-token", []byte("alice-value"))
	require.NoError(t, err)

	var stored []byte
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT ciphertext FROM user_secrets WHERE principal = 'alice' AND name = 'github-token'").Scan(&stored))
	assert.NotContains(t, string(stored), "alice-value", "the row holds no plaintext")

	value, _, err := broker.Get(t.Context(), "alice", "github-token")
	require.NoError(t, err)
	assert.Equal(t, "alice-value", string(value))

	_, _, err = broker.Get(t.Context(), "bob", "github-token")
	require.ErrorIs(t, err, secrets.ErrNotFound)

	listed, err := broker.List(t.Context(), "bob")
	require.NoError(t, err)
	assert.Empty(t, listed)

	require.ErrorIs(t, broker.Delete(t.Context(), "bob", "github-token"), secrets.ErrNotFound)

	listed, err = broker.List(t.Context(), "alice")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "github-token", listed[0].Name)

	require.NoError(t, broker.Delete(t.Context(), "alice", "github-token"))

	_, _, err = broker.Get(t.Context(), "alice", "github-token")
	require.ErrorIs(t, err, secrets.ErrNotFound)
}
