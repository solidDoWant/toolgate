//go:build integration

package runtime_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// expiringInstance inserts an enabled instance in status whose resolved
// value expires at expiresAt.
func (f *stopFixture) expiringInstance(t *testing.T, name, status string, expiresAt time.Time, enabled bool) (*v1alpha1.MCPServerTemplate, db.Instance) {
	t.Helper()

	template := f.template(name, "per-user", time.Hour, v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"})
	session := f.session(t, name+"-user", "s1", "", "staff")

	if enabled {
		f.enable(t, session, template)
	}

	instance := f.instance(t, template, "user:"+name+"-user", name+"-user", status)
	_, err := f.pool.Exec(t.Context(), "UPDATE instances SET resolved_expires_at = $2, last_used_at = now() WHERE id = $1", instance.ID, expiresAt)
	require.NoError(t, err)

	return template, f.get(t, instance.ID)
}

// expiryPass runs the stop and resolver-expiry steps once at the fixture's
// clock.
func (f *stopFixture) expiryPass(t *testing.T) {
	t.Helper()

	replacer := f.replacer()
	replacer.Now = func() time.Time { return *f.now.Load() }
	pass := runtime.FullPass(f.queries, slog.New(slog.DiscardHandler), replacer.ExpiryStep)
	require.NoError(t, pass(t.Context()))
}

func (f *stopFixture) expiryStops(t *testing.T, instance db.Instance) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = 'instance.stop' AND details->>'instance' = $1 AND details->>'reason' = 'resolver-expiry'",
		instance.ID).Scan(&count))

	return count
}

func TestResolverExpiryReplacement(t *testing.T) {
	fixture := newStopFixture(t)
	now := time.Now().Truncate(time.Second)
	fixture.setNow(now)

	for _, status := range []string{"ready", "starting"} {
		t.Run("[row 58] a "+status+" instance under 60 s from expiry is replaced", func(t *testing.T) {
			_, old := fixture.expiringInstance(t, "expiring-"+status, status, now.Add(59*time.Second), true)

			fixture.expiryPass(t)

			assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)
			replacement, found := fixture.liveInstance(t, old)
			require.True(t, found, "a new live row")
			assert.NotEqual(t, old.ID, replacement.ID)
			assert.Equal(t, "pending", replacement.Status)
			assert.False(t, replacement.ResolvedExpiresAt.Valid, "the new row resolves again")
			assert.Equal(t, 1, fixture.expiryStops(t, old))
		})
	}

	t.Run("an instance exactly 60 s from expiry is kept", func(t *testing.T) {
		_, old := fixture.expiringInstance(t, "expiring-later", "ready", now.Add(60*time.Second), true)

		fixture.expiryPass(t)

		live, found := fixture.liveInstance(t, old)
		require.True(t, found)
		assert.Equal(t, old.ID, live.ID)
		assert.Zero(t, fixture.expiryStops(t, old))
	})

	t.Run("an instance without resolved values is kept", func(t *testing.T) {
		template := fixture.template("no-resolvers", "per-user", time.Hour, v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"})
		old := fixture.instance(t, template, "user:no-resolvers-user", "no-resolvers-user", "ready")

		fixture.expiryPass(t)

		live, found := fixture.liveInstance(t, old)
		require.True(t, found)
		assert.Equal(t, old.ID, live.ID)
	})

	t.Run("[row 72] an idle instance with no enabling session is stopped, not replaced", func(t *testing.T) {
		_, old := fixture.expiringInstance(t, "expiring-idle", "ready", now.Add(30*time.Second), false)

		fixture.setNow(now.Add(2 * time.Hour))
		defer fixture.setNow(now)

		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET resolved_expires_at = $2 WHERE id = $1", old.ID, now.Add(2*time.Hour+30*time.Second))
		require.NoError(t, err)

		fixture.expiryPass(t)

		assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)
		_, found := fixture.liveInstance(t, old)
		assert.False(t, found, "no replacement")
		assert.Zero(t, fixture.expiryStops(t, old))
	})

	t.Run("[row 73] an instance whose template was deleted is stopped, not replaced", func(t *testing.T) {
		template, old := fixture.expiringInstance(t, "expiring-deleted", "ready", now.Add(30*time.Second), true)
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		fixture.expiryPass(t)

		assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)
		_, found := fixture.liveInstance(t, old)
		assert.False(t, found, "no replacement")
		assert.Zero(t, fixture.expiryStops(t, old))
	})

	t.Run("[row 73] an instance whose owner lost access is stopped, not replaced", func(t *testing.T) {
		template, old := fixture.expiringInstance(t, "expiring-access", "ready", now.Add(30*time.Second), true)
		template.Spec.Access.Groups = []string{"admins"}
		fixture.newVersion(template)

		fixture.expiryPass(t)

		assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)
		_, found := fixture.liveInstance(t, old)
		assert.False(t, found, "no replacement")
		assert.Zero(t, fixture.expiryStops(t, old))
	})
}

func TestMarkStartingRecordsResolvedExpiry(t *testing.T) {
	fixture := newStopFixture(t)
	template := fixture.template("mark-expiry", "per-user", time.Hour, v1alpha1.Access{Groups: []string{"staff"}})
	instance := fixture.instance(t, template, "user:mark-expiry-user", "mark-expiry-user", "pending")

	expiresAt := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	changed, err := runtime.PostgresStartStore{Pool: fixture.pool}.MarkStarting(t.Context(), instance.ID, 1, nil, expiresAt)
	require.NoError(t, err)
	require.True(t, changed)

	row := fixture.get(t, instance.ID)
	require.True(t, row.ResolvedExpiresAt.Valid)
	assert.True(t, expiresAt.Equal(row.ResolvedExpiresAt.Time))
}
