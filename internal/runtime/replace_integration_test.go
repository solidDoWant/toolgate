//go:build integration

package runtime_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// replacer returns a Replacer over the fixture, as a reconciler that just
// gained the Lease builds it.
func (f *stopFixture) replacer() *runtime.Replacer {
	now := func() time.Time { return *f.now.Load() }
	logger := slog.New(slog.DiscardHandler)

	return &runtime.Replacer{
		Pool:  f.pool,
		Stops: &runtime.StopDecider{Pool: f.pool, Templates: f.catalog, Log: logger, Now: now},
		Log:   logger,
	}
}

// requestRestart records a restart request for the instance, as the web UI
// does.
func (f *stopFixture) requestRestart(t *testing.T, instance db.Instance, user string) {
	t.Helper()

	changed, err := f.queries.RequestInstanceRestart(t.Context(), db.RequestInstanceRestartParams{
		ID: instance.ID, RequestedBy: pgtype.Text{String: user, Valid: true},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
}

// newVersion makes the template's next generation the running one.
func (f *stopFixture) newVersion(template *v1alpha1.MCPServerTemplate) {
	template.Generation++
	f.catalog.Set(template, nil)
}

func (f *stopFixture) liveInstance(t *testing.T, instance db.Instance) (db.Instance, bool) {
	t.Helper()

	live, err := f.queries.GetLiveInstance(t.Context(), db.GetLiveInstanceParams{
		TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName, ScopeKey: instance.ScopeKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Instance{}, false
	}

	require.NoError(t, err)

	return live, true
}

// restartStops counts the instance.stop events with reason restart for the
// instance.
func (f *stopFixture) restartStops(t *testing.T, instance db.Instance) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = 'instance.stop' AND details->>'instance' = $1 AND details->>'reason' = 'restart'",
		instance.ID).Scan(&count))

	return count
}

func TestReplace(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"}

	t.Run("a new leader's first pass replaces a requested instance with one at the current generation", func(t *testing.T) {
		template := fixture.template("replace", "per-user", time.Hour, everyone)
		session := fixture.session(t, "replace-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		old := fixture.instance(t, template, "user:replace-user", "replace-user", "ready")
		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET render_session = $2 WHERE id = $1", old.ID, session.ID)
		require.NoError(t, err)
		old = fixture.get(t, old.ID)

		fixture.requestRestart(t, old, "replace-user")
		fixture.newVersion(template)

		// The request was written before this reconciler existed.
		replacer := fixture.replacer()
		pass := runtime.FullPass(fixture.queries, slog.New(slog.DiscardHandler), replacer.Stops.Step, replacer.Step)
		require.NoError(t, pass(t.Context()))

		assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)

		replacement, found := fixture.liveInstance(t, old)
		require.True(t, found, "a new live row for the template and scope key")
		assert.NotEqual(t, old.ID, replacement.ID)
		assert.Equal(t, "pending", replacement.Status)
		assert.Equal(t, "running", replacement.Desired)
		assert.Equal(t, old.Scope, replacement.Scope)
		assert.Equal(t, old.OwnerPrincipal, replacement.OwnerPrincipal)
		assert.Equal(t, old.RenderSession, replacement.RenderSession)
		assert.Equal(t, template.Generation, replacement.Generation)
		assert.Equal(t, string(template.UID), replacement.TemplateUid)
		assert.Equal(t, "tg-"+replacement.ID, replacement.Namespace)
		assert.True(t, replacement.AttemptStartedAt.Valid)
		require.True(t, replacement.LastUsedAt.Valid, "last_used_at is carried over")
		assert.WithinDuration(t, old.ReadyAt.Time, replacement.LastUsedAt.Time, time.Microsecond,
			"an unused instance carries over ready_at")
		assert.False(t, replacement.RestartRequestedAt.Valid)
		assert.Equal(t, 1, fixture.restartStops(t, old))

		var principal string
		require.NoError(t, fixture.pool.QueryRow(t.Context(),
			"SELECT principal FROM audit_events WHERE type = 'instance.stop' AND details->>'instance' = $1", old.ID).Scan(&principal))
		assert.Equal(t, "replace-user", principal)

		// The next pass doesn't replace it again.
		require.NoError(t, pass(t.Context()))

		again, found := fixture.liveInstance(t, old)
		require.True(t, found)
		assert.Equal(t, replacement.ID, again.ID)
	})

	t.Run("a replacement carries over last_used_at", func(t *testing.T) {
		template := fixture.template("replace-used", "per-user", time.Hour, everyone)
		session := fixture.session(t, "replace-used-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		old := fixture.instance(t, template, "user:replace-used-user", "replace-used-user", "ready")

		used := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET last_used_at = $2 WHERE id = $1", old.ID, used)
		require.NoError(t, err)

		replacement, err := fixture.replacer().Replace(t.Context(), fixture.get(t, old.ID), runtime.ReplaceReasonResolverExpiry, "")
		require.NoError(t, err)
		require.True(t, replacement.LastUsedAt.Valid)
		assert.True(t, used.Equal(replacement.LastUsedAt.Time))
	})

	t.Run("a stop condition stops the instance instead of replacing it", func(t *testing.T) {
		template := fixture.template("replace-idle", "per-user", time.Minute, everyone)
		old := fixture.instance(t, template, "user:replace-idle-user", "replace-idle-user", "ready")
		fixture.requestRestart(t, old, "replace-idle-user")
		fixture.newVersion(template)

		// Nobody enables it and it is idle.
		fixture.setNow(old.ReadyAt.Time.Add(2 * time.Minute))
		defer fixture.setNow(time.Now())

		replacement, err := fixture.replacer().Replace(t.Context(), old, runtime.ReplaceReasonRestart, "replace-idle-user")
		require.NoError(t, err)
		assert.Empty(t, replacement.ID)

		assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)

		_, found := fixture.liveInstance(t, old)
		assert.False(t, found, "no replacement")
		assert.Zero(t, fixture.restartStops(t, old))
	})

	t.Run("a replacement waits for the advisory lock and does nothing when the stop commits first", func(t *testing.T) {
		template := fixture.template("replace-race", "per-user", time.Hour, everyone)
		session := fixture.session(t, "replace-race-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		old := fixture.instance(t, template, "user:replace-race-user", "replace-race-user", "ready")
		fixture.requestRestart(t, old, "replace-race-user")

		// An idle stop holds the advisory lock and hasn't committed yet.
		stop, err := fixture.pool.Begin(t.Context())
		require.NoError(t, err)

		defer func() { _ = stop.Rollback(context.WithoutCancel(t.Context())) }()

		stopQueries := db.New(stop)
		require.NoError(t, stopQueries.LockInstanceScope(t.Context(), db.LockInstanceScopeParams{
			TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: old.ScopeKey,
		}))
		_, err = stopQueries.SetInstanceDesiredStopped(t.Context(), old.ID)
		require.NoError(t, err)

		type result struct {
			replacement db.Instance
			err         error
		}

		replaced := make(chan result, 1)

		go func() {
			replacement, err := fixture.replacer().Replace(context.WithoutCancel(t.Context()), old, runtime.ReplaceReasonRestart, "replace-race-user")
			replaced <- result{replacement, err}
		}()

		require.Eventually(t, func() bool {
			var waiting int

			err := fixture.pool.QueryRow(t.Context(),
				"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted").Scan(&waiting)

			return err == nil && waiting == 1
		}, 10*time.Second, 10*time.Millisecond, "the replacement should wait for the advisory lock")

		require.NoError(t, stop.Commit(t.Context()))

		outcome := <-replaced
		require.NoError(t, outcome.err)
		assert.Empty(t, outcome.replacement.ID)

		_, found := fixture.liveInstance(t, old)
		assert.False(t, found, "no replacement of a stopped instance")
		assert.Zero(t, fixture.restartStops(t, old))
	})

	t.Run("when the replacement commits first the stop's re-check runs on the new row", func(t *testing.T) {
		template := fixture.template("replace-first", "per-user", time.Minute, everyone)
		session := fixture.session(t, "replace-first-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		old := fixture.instance(t, template, "user:replace-first-user", "replace-first-user", "ready")

		replacement, err := fixture.replacer().Replace(t.Context(), old, runtime.ReplaceReasonRestart, "replace-first-user")
		require.NoError(t, err)
		require.NotEmpty(t, replacement.ID)

		// The stop decision of the old row's snapshot re-reads it under the
		// lock and finds it not live.
		stops := fixture.replacer().Stops
		current, err := stops.Decide(t.Context(), old)
		require.NoError(t, err)
		assert.Equal(t, old.ID, current.ID)

		live, found := fixture.liveInstance(t, old)
		require.True(t, found)
		assert.Equal(t, replacement.ID, live.ID)

		// Without enablers and once idle, the new row stops: last_used_at
		// came from the old row, so the replacement didn't count as use.
		_, err = fixture.queries.DisableServer(t.Context(), db.DisableServerParams{
			SessionPk: session.ID, TemplateNs: template.Namespace, TemplateName: template.Name,
		})
		require.NoError(t, err)

		fixture.setNow(old.ReadyAt.Time.Add(2 * time.Minute))
		defer fixture.setNow(time.Now())

		stopped, err := stops.Decide(t.Context(), live)
		require.NoError(t, err)
		assert.Equal(t, "stopped", stopped.Desired)
	})
}
