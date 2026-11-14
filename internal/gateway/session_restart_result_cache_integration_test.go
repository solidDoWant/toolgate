//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/policy"
)

// A restart of an ended session deletes that session's cached results and no
// others, even when the session ended without the end transaction's delete.
func TestSessionRestartDeletesItsCachedResults(t *testing.T) {
	fixture := newAdditionFixture(t)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultNone)

	first := fixture.connectObserved(t, attestedAlice())
	listServers(t, first.session)

	const insert = `INSERT INTO tool_result_cache (key, template_uid, generation, tool, scope, scope_id, session_pk, result, size_bytes, expires_at)
		VALUES ($1, 'uid', 1, 'search', $2, '', $3, '{}', 2, now() + interval '1 hour')`

	var sessionID int64

	require.NoError(t, fixture.pool.QueryRow(t.Context(), `SELECT id FROM sessions WHERE principal = 'alice' AND session_id = 'session-1'`).Scan(&sessionID))

	_, err := fixture.pool.Exec(t.Context(), insert, []byte("0123456789abcdef0123456789abcdef"), "per-session", sessionID)
	require.NoError(t, err)
	_, err = fixture.pool.Exec(t.Context(), insert, []byte("fedcba9876543210fedcba9876543210"), "per-session", sessionID+1000)
	require.NoError(t, err)
	_, err = fixture.pool.Exec(t.Context(), insert, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "shared", nil)
	require.NoError(t, err)

	_, err = fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now() WHERE id = $1`, sessionID)
	require.NoError(t, err)

	second := fixture.connectObserved(t, attestedAlice())
	listServers(t, second.session)

	var remaining []int64

	rows, err := fixture.pool.Query(t.Context(), `SELECT coalesce(session_pk, 0) FROM tool_result_cache ORDER BY 1`)
	require.NoError(t, err)

	for rows.Next() {
		var pk int64
		require.NoError(t, rows.Scan(&pk))

		remaining = append(remaining, pk)
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{0, sessionID + 1000}, remaining, "the restarted session's entries are gone and the others stay")
}
