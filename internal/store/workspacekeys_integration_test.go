//go:build integration

package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// keyIDOf returns a valid 43 character key ID made of one letter.
func keyIDOf(letter string) string { return strings.Repeat(letter, 43) }

// insertKeyRow inserts a workspace key row with the given status and end
// time (a SQL interval before now, or empty for none) and returns its ID.
func insertKeyRow(t *testing.T, pool *pgxpool.Pool, principal, name, keyID, status, endedAgo string) int64 {
	t.Helper()

	ended := "NULL"
	if endedAgo != "" {
		ended = fmt.Sprintf("now() - interval '%s'", endedAgo)
	}

	var id int64

	err := pool.QueryRow(t.Context(), fmt.Sprintf(`INSERT INTO workspace_keys
		(principal, username, name, key_id, public_key, status, request_id, registered_at, approved_at, ended_at)
		VALUES ($1, 'alice', $2, $3, decode(repeat('ab', 32), 'hex'), $4, 'r', now(), now(), %s) RETURNING id`, ended),
		principal, name, keyID, status).Scan(&id)
	require.NoError(t, err)

	return id
}

func keyRowCount(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()

	var count int

	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM workspace_keys WHERE id = $1`, id).Scan(&count))

	return count
}

func TestWorkspaceKeyCleanup(t *testing.T) {
	_, pool := migratedPool(t)

	revokedOld := insertKeyRow(t, pool, "alice", "a", keyIDOf("a"), "revoked", "7 days 1 minute")
	replacedOld := insertKeyRow(t, pool, "alice", "b", keyIDOf("b"), "replaced", "8 days")
	active := insertKeyRow(t, pool, "alice", "c", keyIDOf("c"), "active", "")
	endedRecently := insertKeyRow(t, pool, "alice", "d", keyIDOf("d"), "revoked", "6 days 23 hours 59 minutes")

	require.NoError(t, store.DeleteStaleRows(t.Context(), db.New(pool)))

	assert.Zero(t, keyRowCount(t, pool, revokedOld))
	assert.Zero(t, keyRowCount(t, pool, replacedOld))
	assert.Equal(t, 1, keyRowCount(t, pool, active))
	assert.Equal(t, 1, keyRowCount(t, pool, endedRecently))
}

func TestWorkspaceKeyConstraints(t *testing.T) {
	_, pool := migratedPool(t)

	insert := func(principal, username, name, keyID string, publicKey []byte, status string, ended bool) error {
		endedAt := "NULL"
		if ended {
			endedAt = "now()"
		}

		_, err := pool.Exec(t.Context(), `INSERT INTO workspace_keys
			(principal, username, name, key_id, public_key, status, request_id, registered_at, approved_at, ended_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'r', now(), now(), `+endedAt+`)`,
			principal, username, name, keyID, publicKey, status)

		return err
	}

	good := make([]byte, 32)

	require.NoError(t, insert("alice", "alice", "dev", keyIDOf("a"), good, "active", false))

	tests := []struct {
		name      string
		principal string
		username  string
		keyName   string
		keyID     string
		publicKey []byte
		status    string
		ended     bool
	}{
		{"second active row for the principal and name", "alice", "alice", "dev", keyIDOf("b"), good, "active", false},
		{"second active row for the key ID", "bob", "bob", "dev", keyIDOf("a"), good, "active", false},
		{"bad name", "bob", "bob", "has space", keyIDOf("c"), good, "active", false},
		{"empty name", "bob", "bob", "", keyIDOf("c"), good, "active", false},
		{"bad username", "bob", "b/b", "dev", keyIDOf("c"), good, "active", false},
		{"short key ID", "bob", "bob", "dev", strings.Repeat("c", 42), good, "active", false},
		{"key ID with padding", "bob", "bob", "dev", strings.Repeat("c", 42) + "=", good, "active", false},
		{"short public key", "bob", "bob", "dev", keyIDOf("c"), good[:31], "active", false},
		{"long public key", "bob", "bob", "dev", keyIDOf("c"), append(good[:32:32], 0), "active", false},
		{"active row with ended_at", "bob", "bob", "dev", keyIDOf("c"), good, "active", true},
		{"revoked row without ended_at", "bob", "bob", "dev", keyIDOf("c"), good, "revoked", false},
		{"unknown status", "bob", "bob", "dev", keyIDOf("c"), good, "pending", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, insert(test.principal, test.username, test.keyName, test.keyID, test.publicKey, test.status, test.ended))
		})
	}

	t.Run("ended rows may repeat a name and key ID", func(t *testing.T) {
		require.NoError(t, insert("alice", "alice", "dev", keyIDOf("a"), good, "revoked", true))
		require.NoError(t, insert("alice", "alice", "dev", keyIDOf("a"), good, "replaced", true))
	})
}

func TestGetWorkspaceKeyByKeyID(t *testing.T) {
	_, pool := migratedPool(t)
	keys := store.NewWorkspaceKeys(pool)

	_, found, err := keys.WorkspaceKey(t.Context(), keyIDOf("z"))
	require.NoError(t, err)
	assert.False(t, found)

	older := insertKeyRow(t, pool, "alice", "dev", keyIDOf("a"), "revoked", "3 days")
	newer := insertKeyRow(t, pool, "alice", "dev", keyIDOf("a"), "replaced", "1 day")

	row, found, err := keys.WorkspaceKey(t.Context(), keyIDOf("a"))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, newer, row.ID, "the newest ended row, not the older one %d", older)
	assert.Equal(t, "replaced", row.Status)
	assert.Len(t, row.PublicKey, 32)
	assert.Nil(t, row.LastUsedAt)

	active := insertKeyRow(t, pool, "alice", "dev", keyIDOf("a"), "active", "")

	row, found, err = keys.WorkspaceKey(t.Context(), keyIDOf("a"))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, active, row.ID)
	assert.Equal(t, "active", row.Status)
	assert.Equal(t, "alice", row.Principal)
	assert.Equal(t, "dev", row.Name)
	assert.Equal(t, "alice", row.Username)
}

func TestTouchWorkspaceKeyWritesAtMostOncePerMinute(t *testing.T) {
	_, pool := migratedPool(t)
	keys := store.NewWorkspaceKeys(pool)
	id := insertKeyRow(t, pool, "alice", "dev", keyIDOf("a"), "active", "")
	ended := insertKeyRow(t, pool, "alice", "old", keyIDOf("b"), "revoked", "1 day")

	lastUsed := func(id int64) *string {
		var value *string

		require.NoError(t, pool.QueryRow(t.Context(), `SELECT last_used_at::text FROM workspace_keys WHERE id = $1`, id).Scan(&value))

		return value
	}

	require.NoError(t, keys.TouchWorkspaceKey(t.Context(), id))

	first := lastUsed(id)
	require.NotNil(t, first)

	require.NoError(t, keys.TouchWorkspaceKey(t.Context(), id))
	assert.Equal(t, first, lastUsed(id), "a second touch inside a minute writes nothing")

	_, err := pool.Exec(t.Context(), `UPDATE workspace_keys SET last_used_at = now() - interval '61 seconds' WHERE id = $1`, id)
	require.NoError(t, err)
	require.NoError(t, keys.TouchWorkspaceKey(t.Context(), id))

	var recent bool

	require.NoError(t, pool.QueryRow(t.Context(), `SELECT last_used_at > now() - interval '10 seconds' FROM workspace_keys WHERE id = $1`, id).Scan(&recent))
	assert.True(t, recent, "a touch after a minute writes")

	require.NoError(t, keys.TouchWorkspaceKey(t.Context(), ended))
	assert.Nil(t, lastUsed(ended), "an ended key is not touched")
}

func TestHasPendingKeyRequest(t *testing.T) {
	_, pool := migratedPool(t)
	keys := store.NewWorkspaceKeys(pool)

	insertRequest := func(id, principal, kind, status, expires, keyID string) {
		t.Helper()

		_, err := pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO access_requests (id, principal, kind, workspace, verified, change, status, created_at, expires_at)
			VALUES ($1, $2, $3, 'claimed/default', false, jsonb_build_object('name', 'dev', 'keyID', $4::text), $5, now() - interval '2 hours', now() + interval '%s')`, expires),
			id, principal, kind, keyID, status)
		require.NoError(t, err)
	}

	hex := func(n int) string { return fmt.Sprintf("%032x", n) }

	insertRequest(hex(1), "alice", "key", "pending", "1 hour", keyIDOf("a"))
	insertRequest(hex(2), "alice", "key", "pending", "-1 hour", keyIDOf("b"))
	insertRequest(hex(3), "alice", "key", "approved", "1 hour", keyIDOf("c"))
	insertRequest(hex(4), "alice", "key", "denied", "1 hour", keyIDOf("d"))
	insertRequest(hex(5), "alice", "change", "pending", "1 hour", keyIDOf("e"))

	for keyID, want := range map[string]bool{
		keyIDOf("a"): true, keyIDOf("b"): false, keyIDOf("c"): false, keyIDOf("d"): false, keyIDOf("e"): false, keyIDOf("f"): false,
	} {
		got, err := keys.HasPendingKeyRequest(t.Context(), "alice", keyID)
		require.NoError(t, err)
		assert.Equal(t, want, got, keyID)
	}

	other, err := keys.HasPendingKeyRequest(t.Context(), "bob", keyIDOf("a"))
	require.NoError(t, err)
	assert.False(t, other, "another principal's request does not count")
}

func TestInsertKeyRequestWithinLimit(t *testing.T) {
	_, pool := migratedPool(t)
	queries := db.New(pool)

	insert := func(id string, limit int64) error {
		_, err := queries.InsertKeyRequestWithinLimit(t.Context(), db.InsertKeyRequestWithinLimitParams{
			ID: id, Principal: "alice", Workspace: "claimed/default", Change: []byte(`{"name":"dev"}`),
			ApprovalTimeout: interval(time.Hour), PendingLimit: limit,
		})

		return err
	}

	require.NoError(t, insert(fmt.Sprintf("%032x", 1), 1))
	require.Error(t, insert(fmt.Sprintf("%032x", 2), 1), "at the limit no row is inserted")
	require.NoError(t, insert(fmt.Sprintf("%032x", 3), 2))
}
