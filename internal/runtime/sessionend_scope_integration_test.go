//go:build integration

package runtime_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// addSessionScopeRows gives the session an allow entry, a deny entry and a
// found tool.
func (f *stopFixture) addSessionScopeRows(t *testing.T, session db.Session) {
	t.Helper()

	require.NoError(t, store.EnsureUserScope(t.Context(), f.pool, session.Principal))

	for _, effect := range []string{"allow", "deny"} {
		_, err := f.pool.Exec(t.Context(), `INSERT INTO scope_entries (scope_id, session_pk, server, tool, effect)
			SELECT id, $2, 'github', 'create_issue', $3 FROM user_scopes WHERE principal = $1`, session.Principal, session.ID, effect)
		require.NoError(t, err)
	}

	_, err := f.pool.Exec(t.Context(), `INSERT INTO session_found_tools (session_pk, template_ns, template_name, upstream_name)
		VALUES ($1, 'catalog', 'github', 'create_issue')`, session.ID)
	require.NoError(t, err)
}

// sessionEntryEffects returns the effects of the session's scope entries,
// sorted.
func (f *stopFixture) sessionEntryEffects(t *testing.T, session db.Session) []string {
	t.Helper()

	var effects []string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT coalesce(array_agg(effect ORDER BY effect), '{}') FROM scope_entries WHERE session_pk = $1`, session.ID).Scan(&effects))

	return effects
}

func (f *stopFixture) foundToolCount(t *testing.T, session db.Session) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_found_tools WHERE session_pk = $1`, session.ID).Scan(&count))

	return count
}

func TestSessionScopeEntriesEndAndForget(t *testing.T) {
	fixture := newStopFixture(t)

	t.Run("rows 229, 240: ending deletes the allow entries and found tools, and forgetting deletes the deny entries", func(t *testing.T) {
		session := fixture.session(t, "entries-user", "s1", "", "staff")
		fixture.addSessionScopeRows(t, session)

		require.NoError(t, runtime.EndSession(t.Context(), fixture.pool, nil, session.ID, "closed"))

		assert.Equal(t, []string{"deny"}, fixture.sessionEntryEffects(t, session), "the deny entry outlives the end")
		assert.Zero(t, fixture.foundToolCount(t, session))

		fixture.endedAt(t, session, sessionResumeWindow-time.Minute)
		fixture.endIdleSessions(t)
		assert.Equal(t, []string{"deny"}, fixture.sessionEntryEffects(t, session), "a session in its resume window keeps its deny entry")
		assert.Empty(t, fixture.auditServers(t, "session.forget", session))

		fixture.endedAt(t, session, sessionResumeWindow+time.Minute)
		fixture.endIdleSessions(t)
		assert.Empty(t, fixture.sessionEntryEffects(t, session), "session.forget deletes the deny entry")
		assert.Equal(t, [][]string{{}}, fixture.auditServers(t, "session.forget", session))
	})

	t.Run("row 247: the cleanup pass deletes the allow entries and found tools of a session ended without them deleted", func(t *testing.T) {
		ended := fixture.session(t, "old-leader-user", "s1", "", "staff")
		live := fixture.session(t, "old-leader-user", "s2", "", "staff")
		fixture.addSessionScopeRows(t, ended)
		fixture.addSessionScopeRows(t, live)

		// A 0.3 leader ends a session without touching its entries.
		_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now() WHERE id = $1`, ended.ID)
		require.NoError(t, err)

		require.NoError(t, store.DeleteStaleRows(t.Context(), db.New(fixture.pool)))

		assert.Equal(t, []string{"deny"}, fixture.sessionEntryEffects(t, ended))
		assert.Zero(t, fixture.foundToolCount(t, ended))
		assert.Equal(t, []string{"allow", "deny"}, fixture.sessionEntryEffects(t, live), "a live session keeps its entries")
		assert.Equal(t, 1, fixture.foundToolCount(t, live))
	})
}
