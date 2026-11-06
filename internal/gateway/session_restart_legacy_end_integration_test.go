//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Row 247 / row 240: a session ended by a 0.3 leader keeps its allow
// entries until the 0.4 leader's pass. If the session restarts (resume)
// before that pass, the entries must still not apply to the restarted
// session, and the pass must still remove them.
func TestSessionRestartAfterLegacyEndClearsAllowEntries(t *testing.T) {
	fixture := newAdditionFixture(t)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultNone)

	first := fixture.connectObserved(t, attestedAlice())
	assertBlockedInWorkspace(t, listServers(t, first.session), "github")

	fixture.addSessionEntry(t, "session-1", "github", "echo", policy.EffectAllow)
	fixture.addSessionEntry(t, "session-1", "github", "other", policy.EffectDeny)
	assertNotBlocked(t, listServers(t, first.session), "github")

	// A 0.3 leader ends the session: it sets ended_at and deletes the
	// session additions, and knows nothing of scope_entries.
	_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now() WHERE principal = 'alice' AND session_id = 'session-1'`)
	require.NoError(t, err)
	_, err = fixture.pool.Exec(t.Context(), `DELETE FROM session_additions`)
	require.NoError(t, err)

	// The client comes back with the same session ID: the session restarts.
	second := fixture.connectObserved(t, attestedAlice())
	listServers(t, second.session)

	var restarted bool
	require.NoError(t, fixture.pool.QueryRow(t.Context(), `SELECT ended_at IS NULL FROM sessions WHERE principal = 'alice' AND session_id = 'session-1'`).Scan(&restarted))
	require.True(t, restarted, "the session restarted")

	// The 0.4 leader's 30 s pass runs.
	require.NoError(t, store.DeleteStaleRows(t.Context(), db.New(fixture.pool)))

	t.Logf("scope entries after restart and pass: %v", fixture.scopeEntryRows(t))
	assert.Equal(t, []string{"session:session-1 github other deny"}, fixture.scopeEntryRows(t), "only the deny entry stays")
	assertBlockedInWorkspace(t, listServers(t, second.session), "github")
}
