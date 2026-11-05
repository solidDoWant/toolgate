//go:build integration

package runtime_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const sessionIdleTimeout = 2 * time.Hour

// endIdleSessions runs the session ender with a two-hour idle timeout.
func (f *stopFixture) endIdleSessions(t *testing.T) {
	t.Helper()

	ender := &runtime.SessionEnder{
		Pool:        f.pool,
		IdleTimeout: sessionIdleTimeout,
		Log:         slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	require.NoError(t, ender.Run(t.Context()))
}

// lastSeen moves the session's last_seen_at to ago before the database's
// clock.
func (f *stopFixture) lastSeen(t *testing.T, session db.Session, ago time.Duration) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE sessions SET last_seen_at = now() - make_interval(secs => $2) WHERE id = $1",
		session.ID, ago.Seconds())
	require.NoError(t, err)
}

func (f *stopFixture) sessionEnds(t *testing.T, session db.Session) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = 'session.end' AND principal = $1 AND session = $2",
		session.Principal, session.SessionID).Scan(&count))

	return count
}

func TestAC1_15SessionIdleExpiry(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}}

	t.Run("[AC-1-15] an idle session ends and its per-session instance stops", func(t *testing.T) {
		template := fixture.template("idle-session", "per-session", 0, everyone)
		session := fixture.session(t, "idle-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "session:idle-user:s1", "idle-user", "ready")

		fixture.lastSeen(t, session, sessionIdleTimeout+time.Minute)
		fixture.endIdleSessions(t)

		ended, err := fixture.queries.GetSessionByID(t.Context(), session.ID)
		require.NoError(t, err)
		require.True(t, ended.EndedAt.Valid, "the idle session should have ended")

		enabled, err := fixture.queries.ListSessionServers(t.Context(), session.ID)
		require.NoError(t, err)
		require.Empty(t, enabled, "ending a session deletes its enabled set")
		require.Equal(t, 1, fixture.sessionEnds(t, session), "ending a session writes session.end")

		fixture.pass(t)
		fixture.requireTornDown(t, instance)

		fixture.endIdleSessions(t)
		require.Equal(t, 1, fixture.sessionEnds(t, session), "an ended session isn't ended again")
	})

	t.Run("[AC-1-15] a session with a request inside the idle timeout stays", func(t *testing.T) {
		template := fixture.template("active-session", "per-session", 0, everyone)
		session := fixture.session(t, "active-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "session:active-user:s1", "active-user", "ready")

		fixture.lastSeen(t, session, sessionIdleTimeout-time.Minute)
		fixture.endIdleSessions(t)
		fixture.pass(t)

		current, err := fixture.queries.GetSessionByID(t.Context(), session.ID)
		require.NoError(t, err)
		require.False(t, current.EndedAt.Valid)

		enabled, err := fixture.queries.ListSessionServers(t.Context(), session.ID)
		require.NoError(t, err)
		require.Len(t, enabled, 1)
		require.Zero(t, fixture.sessionEnds(t, session))
		fixture.requireRunning(t, instance)
	})

	t.Run("[AC-1-15] a per-user instance stops after its idleStop once its owner's session ended", func(t *testing.T) {
		template := fixture.template("idle-user-server", "per-user", 30*time.Minute, everyone)
		session := fixture.session(t, "owner", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "user:owner", "owner", "ready")
		start := time.Now()

		fixture.lastSeen(t, session, sessionIdleTimeout+time.Minute)
		fixture.endIdleSessions(t)

		fixture.setNow(start.Add(29 * time.Minute))
		fixture.pass(t)
		fixture.requireRunning(t, instance)

		fixture.setNow(start.Add(31 * time.Minute))
		fixture.pass(t)
		fixture.requireTornDown(t, instance)
	})
}
