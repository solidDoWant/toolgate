//go:build integration

package gateway_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// readyInstance stores a ready instance of template at generation, owned by
// owner (empty for shared), and returns its ID.
func (f *mcpFixture) readyInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, owner string, generation int64) string {
	t.Helper()

	id := f.insertOwnedInstance(t, template, owner, generation)
	queries := db.New(f.pool)

	_, err := queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: id, Generation: generation})
	require.NoError(t, err)
	_, err = queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: id, Tools: []byte(`[]`)})
	require.NoError(t, err)

	return id
}

func TestRequestRestart(t *testing.T) {
	fixture := newMCPFixture(t)
	// Each instance gets its own template: only one instance of a template
	// and scope key is live.
	perUser := func(name string) *v1alpha1.MCPServerTemplate {
		return fixture.addTemplate(name, "per-user", v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"})
	}
	shared := fixture.addTemplate("restart-shared", "shared", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})
	view := gateway.NewCatalogView(fixture.pool, fixture.catalog, &policy.Engine{Rules: fixture.rules})
	queries := db.New(fixture.pool)

	requested := func(t *testing.T, id string) (bool, string) {
		t.Helper()

		row, err := queries.GetInstance(t.Context(), id)
		require.NoError(t, err)

		return row.RestartRequestedAt.Valid, row.RestartRequestedBy.String
	}

	t.Run("the owner restarts an outdated ready instance, notifying it", func(t *testing.T) {
		template := perUser("restart-owner")
		id := fixture.readyInstance(t, template, "alice", template.Generation-1)

		listener := store.NewListener(fixture.pool, slog.New(slog.DiscardHandler))
		events, unsubscribe := listener.Subscribe()
		t.Cleanup(unsubscribe)

		go func() { _ = listener.Run(t.Context()) }()

		select {
		case <-listener.Listening():
		case <-time.After(10 * time.Second):
			t.Fatal("the listener didn't start listening")
		}

		require.NoError(t, view.RequestRestart(t.Context(), id, "alice", false))

		set, by := requested(t, id)
		assert.True(t, set)
		assert.Equal(t, "alice", by)

		deadline := time.After(10 * time.Second)

		for {
			select {
			case event := <-events:
				if event.Kind == store.KindInstance && event.ID == id {
					return
				}
			case <-deadline:
				t.Fatal("no instance notification for the restart request")
			}
		}
	})

	t.Run("another user's instance is not found", func(t *testing.T) {
		template := perUser("restart-other-user")
		id := fixture.readyInstance(t, template, "alice", template.Generation-1)

		require.ErrorIs(t, view.RequestRestart(t.Context(), id, "bob", false), gateway.ErrInstanceNotFound)
		require.ErrorIs(t, view.RequestRestart(t.Context(), id, "bob", true), gateway.ErrInstanceNotFound,
			"admins restart only shared instances of others")

		set, _ := requested(t, id)
		assert.False(t, set)
	})

	t.Run("a shared instance needs an admin", func(t *testing.T) {
		id := fixture.readyInstance(t, shared, "", shared.Generation-1)

		require.ErrorIs(t, view.RequestRestart(t.Context(), id, "alice", false), gateway.ErrInstanceNotFound)

		set, _ := requested(t, id)
		require.False(t, set)

		require.NoError(t, view.RequestRestart(t.Context(), id, "admin", true))

		set, by := requested(t, id)
		assert.True(t, set)
		assert.Equal(t, "admin", by)
	})

	t.Run("an unknown instance is not found", func(t *testing.T) {
		require.ErrorIs(t, view.RequestRestart(t.Context(), "0000", "alice", true), gateway.ErrInstanceNotFound)
	})

	t.Run("an instance that is current, not ready or not live has changed", func(t *testing.T) {
		template := perUser("restart-current")
		current := fixture.readyInstance(t, template, "alice", template.Generation)
		require.ErrorIs(t, view.RequestRestart(t.Context(), current, "alice", false), gateway.ErrInstanceChanged)

		template = perUser("restart-pending")
		pending := fixture.insertOwnedInstance(t, template, "alice", template.Generation-1)
		require.ErrorIs(t, view.RequestRestart(t.Context(), pending, "alice", false), gateway.ErrInstanceChanged)

		template = perUser("restart-stopped")
		stopped := fixture.readyInstance(t, template, "alice", template.Generation-1)
		_, err := queries.SetInstanceDesiredStopped(t.Context(), stopped)
		require.NoError(t, err)
		require.ErrorIs(t, view.RequestRestart(t.Context(), stopped, "alice", false), gateway.ErrInstanceChanged)

		for _, id := range []string{current, pending, stopped} {
			set, _ := requested(t, id)
			assert.False(t, set, id)
		}
	})

	t.Run("[row 55] a second request before the replacement sets the column again; after it, matches nothing", func(t *testing.T) {
		template := perUser("restart-twice")
		id := fixture.readyInstance(t, template, "alice", template.Generation-1)

		require.NoError(t, view.RequestRestart(t.Context(), id, "alice", false))
		require.NoError(t, view.RequestRestart(t.Context(), id, "alice", false))

		// The replacement makes the old row not live.
		_, err := queries.SetInstanceDesiredStopped(t.Context(), id)
		require.NoError(t, err)

		require.ErrorIs(t, view.RequestRestart(t.Context(), id, "alice", false), gateway.ErrInstanceChanged)
	})
}

func TestEnableFollowsAReplacement(t *testing.T) {
	t.Run("[row 53] an enable waiting on a replaced instance waits on the new live row", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.catalog.MarkLoaded()
		template := fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		old := fixture.awaitInstance(t, template, "user:alice")

		require.Eventually(t, func() bool {
			return len(fixture.auditEvents(t, "server.enable", "github")) == 1
		}, waitTimeout, 10*time.Millisecond)

		logger := slog.New(slog.DiscardHandler)
		replacer := &runtime.Replacer{
			Pool:  fixture.pool,
			Stops: &runtime.StopDecider{Pool: fixture.pool, Templates: fixture.catalog, Log: logger},
			Log:   logger,
		}
		replacement, err := replacer.Replace(t.Context(), old, runtime.ReplaceReasonRestart, "alice")
		require.NoError(t, err)
		require.NotEmpty(t, replacement.ID)

		fixture.markReady(t, replacement.ID, "search")

		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, results))
	})
}
