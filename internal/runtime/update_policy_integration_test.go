//go:build integration

package runtime_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func (f *stopFixture) updateStops(t *testing.T, instance db.Instance) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = 'instance.stop' AND details->>'instance' = $1 AND details->>'reason' = 'template-update'",
		instance.ID).Scan(&count))

	return count
}

func TestUpdatePolicyReplacesOutdatedInstances(t *testing.T) {
	fixture := newStopFixture(t)
	now := time.Now().Truncate(time.Second)
	fixture.setNow(now)

	tests := []struct {
		name     string
		policy   *v1alpha1.UpdatePolicy
		idleAgo  time.Duration
		outdated bool
		replaced bool
	}{
		{name: "no-policy", outdated: true, idleAgo: time.Hour},
		{name: "manual", policy: &v1alpha1.UpdatePolicy{Mode: "manual"}, outdated: true, idleAgo: time.Hour},
		{name: "immediate", policy: &v1alpha1.UpdatePolicy{Mode: "immediate"}, outdated: true, replaced: true},
		{name: "immediate-current", policy: &v1alpha1.UpdatePolicy{Mode: "immediate"}},
		{
			name: "idle-at-limit", outdated: true, replaced: true, idleAgo: 5 * time.Minute,
			policy: &v1alpha1.UpdatePolicy{Mode: "whenIdle", IdleFor: &metav1.Duration{Duration: 5 * time.Minute}},
		},
		{
			name: "idle-just-under", outdated: true, idleAgo: 5*time.Minute - time.Second,
			policy: &v1alpha1.UpdatePolicy{Mode: "whenIdle", IdleFor: &metav1.Duration{Duration: 5 * time.Minute}},
		},
		{
			name: "idle-current", idleAgo: time.Hour,
			policy: &v1alpha1.UpdatePolicy{Mode: "whenIdle", IdleFor: &metav1.Duration{Duration: 5 * time.Minute}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := "update-" + test.name
			template := fixture.template(name, "per-user", 24*time.Hour, v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"})
			template.Spec.UpdatePolicy = test.policy
			fixture.catalog.Set(template, nil)
			session := fixture.session(t, name+"-user", "s1", "", "staff")
			fixture.enable(t, session, template)
			old := fixture.instance(t, template, "user:"+name+"-user", name+"-user", "ready")
			_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET last_used_at = $2 WHERE id = $1", old.ID, now.Add(-test.idleAgo))
			require.NoError(t, err)

			if test.outdated {
				fixture.newVersion(template)
			}

			replacer := fixture.replacer()
			replacer.Now = func() time.Time { return now }

			require.NoError(t, replacer.UpdateStep(t.Context(), fixture.get(t, old.ID)))

			if !test.replaced {
				assert.Equal(t, "running", fixture.get(t, old.ID).Desired)
				assert.Zero(t, fixture.updateStops(t, old))

				return
			}

			assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)
			assert.Equal(t, 1, fixture.updateStops(t, old))

			replacement, found := fixture.liveInstance(t, old)
			require.True(t, found)
			assert.Equal(t, template.Generation, replacement.Generation)
			assert.Equal(t, "pending", replacement.Status)
		})
	}
}

func TestReapplyMissingAppliesDeletedObjectsAgain(t *testing.T) {
	fixture := newStopFixture(t)
	now := time.Now().Truncate(time.Second)
	fixture.setNow(now)

	setup := func(t *testing.T, name string, repairs ...time.Time) (db.Instance, client.Client) {
		t.Helper()

		template := fixture.template(name, "per-user", time.Hour, v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"})
		template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
			`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"user":"x"}}`)}}
		fixture.catalog.Set(template, nil)
		session := fixture.session(t, name+"-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "user:"+name+"-user", name+"-user", "ready")
		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET render_session = $2 WHERE id = $1", instance.ID, session.ID)
		require.NoError(t, err)

		for _, repair := range repairs {
			_, err = fixture.pool.Exec(t.Context(), "UPDATE instances SET repairs = array_append(repairs, $2) WHERE id = $1", instance.ID, repair)
			require.NoError(t, err)
		}

		return fixture.get(t, instance.ID), repairKube(t)
	}

	t.Run("a missing object is applied again and counted as a repair", func(t *testing.T) {
		instance, kube := setup(t, "reapply-missing")

		reapplied, err := fixture.repairer(kube).ReapplyMissing(t.Context(), instance)
		require.NoError(t, err)
		assert.True(t, reapplied)

		configMap := &corev1.ConfigMap{}
		require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: "settings"}, configMap))
		assert.Equal(t, 1, fixture.auditCount(t, "instance.reapply", instance.ID))
		assert.Len(t, fixture.get(t, instance.ID).Repairs, 1)
	})

	t.Run("nothing is applied when every object exists", func(t *testing.T) {
		instance, kube := setup(t, "reapply-present")
		require.NoError(t, kube.Create(t.Context(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace, Name: "settings"},
		}))

		reapplied, err := fixture.repairer(kube).ReapplyMissing(t.Context(), instance)
		require.NoError(t, err)
		assert.False(t, reapplied)
		assert.Zero(t, fixture.auditCount(t, "instance.reapply", instance.ID))
	})

	t.Run("an instance with its repairs used up is left to fail", func(t *testing.T) {
		instance, kube := setup(t, "reapply-bound", now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute))

		reapplied, err := fixture.repairer(kube).ReapplyMissing(t.Context(), instance)
		require.NoError(t, err)
		assert.False(t, reapplied)
	})
}
