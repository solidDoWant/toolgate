//go:build integration

package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const unhealthyReason = "GET http://db-proxy:8080/healthz answered 503, expected 200"

// repairer returns a Repairer over the fixture, as a reconciler that just
// gained the Lease builds it, with a Starter that applies to kube.
func (f *stopFixture) repairer(kube client.Client) *runtime.Repairer {
	now := func() time.Time { return *f.now.Load() }
	logger := slog.New(slog.DiscardHandler)

	return &runtime.Repairer{
		Pool:     f.pool,
		Replacer: &runtime.Replacer{Pool: f.pool, Stops: &runtime.StopDecider{Pool: f.pool, Templates: f.catalog, Log: logger, Now: now}, Log: logger, Now: now},
		Starter: &runtime.Starter{
			Store:    runtime.PostgresStartStore{Pool: f.pool},
			Client:   kube,
			Template: f.catalog.Running,
			Secrets: runtime.BrokerSecrets(func(context.Context, string, string) ([]byte, time.Time, error) {
				return nil, time.Time{}, errors.New("no secrets")
			}),
			GatewayNamespace: "toolgate",
			GatewayPodLabels: map[string]string{"app.kubernetes.io/name": "toolgate"},
			Logger:           logger,
		},
		Now: now,
		Log: logger,
	}
}

func repairKube(t *testing.T) client.Client {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	return clientfake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{Apply: reapplyFakeApply}).Build()
}

// reapplyFakeApply is fakeApply, which creates a NetworkPolicy instead of
// applying it, made to accept the second apply of an unchanged policy.
func reapplyFakeApply(ctx context.Context, kube client.WithWatch, object k8sruntime.ApplyConfiguration, options ...client.ApplyOption) error {
	err := fakeApply(ctx, kube, object, options...)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	return err
}

// makeUnhealthy gives a ready instance a stored health check and marks it
// unhealthy since the given time, with the given repair history.
func (f *stopFixture) makeUnhealthy(t *testing.T, instance db.Instance, check v1alpha1.HealthCheck, since time.Time, repairs ...time.Time) db.Instance {
	t.Helper()

	encoded, err := json.Marshal(check)
	require.NoError(t, err)

	history := make([]pgtype.Timestamptz, 0, len(repairs))
	for _, repair := range repairs {
		history = append(history, pgtype.Timestamptz{Time: repair, Valid: true})
	}

	_, err = f.pool.Exec(t.Context(),
		"UPDATE instances SET health_check = $2, unhealthy_since = $3, unhealthy_reason = $4, repairs = $5 WHERE id = $1",
		instance.ID, encoded, since, unhealthyReason, history)
	require.NoError(t, err)

	return f.get(t, instance.ID)
}

func healthCheck(onFailure string, maxRepairs int32) v1alpha1.HealthCheck {
	return v1alpha1.HealthCheck{
		HTTP:             v1alpha1.HealthCheckHTTP{Service: "db-proxy", Port: 8080, Path: "/healthz", ExpectedStatus: 200},
		Interval:         metav1.Duration{Duration: 10 * time.Second},
		Timeout:          metav1.Duration{Duration: time.Second},
		FailureThreshold: 3,
		SuccessThreshold: 1,
		OnFailure:        onFailure,
		MaxRepairs:       maxRepairs,
	}
}

func (f *stopFixture) auditCount(t *testing.T, eventType, instanceID string) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = $1 AND details->>'instance' = $2", eventType, instanceID).Scan(&count))

	return count
}

func (f *stopFixture) healthStops(t *testing.T, instanceID string) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = 'instance.stop' AND details->>'instance' = $1 AND details->>'reason' = 'health-check'",
		instanceID).Scan(&count))

	return count
}

func (f *stopFixture) enabledCount(t *testing.T, template *v1alpha1.MCPServerTemplate) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM session_servers WHERE template_ns = $1 AND template_name = $2", template.Namespace, template.Name).Scan(&count))

	return count
}

func TestRepair(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"}

	// readyUnhealthy enables a template for a new user and returns its
	// ready, unhealthy instance.
	readyUnhealthy := func(t *testing.T, name string, check v1alpha1.HealthCheck, repairs ...time.Time) (*v1alpha1.MCPServerTemplate, db.Instance) {
		t.Helper()

		template := fixture.template(name, "per-user", time.Hour, everyone)
		session := fixture.session(t, name+"-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "user:"+name+"-user", name+"-user", "ready")
		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET render_session = $2 WHERE id = $1", instance.ID, session.ID)
		require.NoError(t, err)

		now := *fixture.now.Load()

		return template, fixture.makeUnhealthy(t, fixture.get(t, instance.ID), check, now.Add(-time.Second), repairs...)
	}

	t.Run("[row 109] replace: a new leader's first pass replaces an unhealthy instance with reason health-check", func(t *testing.T) {
		template, old := readyUnhealthy(t, "repair-replace", healthCheck("replace", 3))

		repairer := fixture.repairer(repairKube(t))
		pass := runtime.FullPass(fixture.queries, slog.New(slog.DiscardHandler), repairer.Step)
		require.NoError(t, pass(t.Context()))

		assert.Equal(t, "stopped", fixture.get(t, old.ID).Desired)
		assert.Equal(t, 1, fixture.healthStops(t, old.ID))

		replacement, found := fixture.liveInstance(t, old)
		require.True(t, found, "a new live row for the template and scope key")
		assert.NotEqual(t, old.ID, replacement.ID)
		assert.Equal(t, "pending", replacement.Status)
		assert.False(t, replacement.UnhealthySince.Valid, "the new row starts healthy")
		require.Len(t, replacement.Repairs, 1, "the repair is counted")
		assert.WithinDuration(t, *fixture.now.Load(), replacement.Repairs[0].Time, time.Millisecond)
		assert.Equal(t, 1, fixture.enabledCount(t, template), "sessions keep the server enabled")

		// The next pass doesn't replace it again.
		require.NoError(t, pass(t.Context()))

		again, found := fixture.liveInstance(t, old)
		require.True(t, found)
		assert.Equal(t, replacement.ID, again.ID)
	})

	t.Run("[row 108] a passing check doesn't make a row under replacement healthy", func(t *testing.T) {
		_, old := readyUnhealthy(t, "repair-passes", healthCheck("", 0))

		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), old))

		changed, err := runtime.PostgresStartStore{Pool: fixture.pool}.SetHealth(t.Context(), fixture.get(t, old.ID), true, "")
		require.NoError(t, err)
		assert.False(t, changed, "the old row is stopping, so the compare-and-set matches nothing")
		assert.Zero(t, fixture.auditCount(t, "instance.health", old.ID))

		replacement, found := fixture.liveInstance(t, old)
		require.True(t, found)
		assert.False(t, replacement.UnhealthySince.Valid)
	})

	t.Run("replace carries the repairs within 30 minutes to the replacement", func(t *testing.T) {
		now := *fixture.now.Load()
		recent := []time.Time{now.Add(-20 * time.Minute), now.Add(-10 * time.Minute)}
		_, old := readyUnhealthy(t, "repair-carry", healthCheck("replace", 3), now.Add(-31*time.Minute), recent[0], recent[1])

		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), old))

		replacement, found := fixture.liveInstance(t, old)
		require.True(t, found)
		require.Len(t, replacement.Repairs, 3, "two recent repairs and this one; the old one is dropped")
		assert.WithinDuration(t, recent[0], replacement.Repairs[0].Time, time.Millisecond)
		assert.WithinDuration(t, recent[1], replacement.Repairs[1].Time, time.Millisecond)
		assert.WithinDuration(t, now, replacement.Repairs[2].Time, time.Millisecond)
	})

	t.Run("[row 110] a repair beyond maxRepairs within 30 minutes fails the row instead", func(t *testing.T) {
		now := *fixture.now.Load()

		// maxRepairs 2 with one repair in the window: the second is made.
		_, allowed := readyUnhealthy(t, "repair-bound-ok", healthCheck("replace", 2), now.Add(-5*time.Minute))
		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), allowed))
		assert.Equal(t, 1, fixture.healthStops(t, allowed.ID))
		assert.Equal(t, "ready", fixture.get(t, allowed.ID).Status)

		// maxRepairs 2 with two repairs in the window: the row fails.
		template, old := readyUnhealthy(t, "repair-bound", healthCheck("replace", 2), now.Add(-5*time.Minute), now.Add(-time.Minute))
		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), old))

		failed := fixture.get(t, old.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "stopped", failed.Desired, "it is torn down")
		assert.Equal(t, "Health check still failing after 2 repairs in 30 minutes: "+unhealthyReason, failed.Reason)
		assert.Equal(t, 1, fixture.auditCount(t, "instance.fail", old.ID))
		assert.Zero(t, fixture.healthStops(t, old.ID))

		_, found := fixture.liveInstance(t, old)
		assert.False(t, found, "no replacement")
		assert.Equal(t, 1, fixture.enabledCount(t, template), "sessions keep the server enabled")

		// The next enable starts with an empty repair history.
		next := fixture.instance(t, template, old.ScopeKey, old.OwnerPrincipal, "pending")
		assert.Empty(t, next.Repairs)
	})

	t.Run("repairs older than 30 minutes don't count against maxRepairs", func(t *testing.T) {
		now := *fixture.now.Load()
		_, old := readyUnhealthy(t, "repair-window", healthCheck("replace", 1), now.Add(-31*time.Minute))

		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), old))

		assert.Equal(t, "ready", fixture.get(t, old.ID).Status)
		assert.Equal(t, 1, fixture.healthStops(t, old.ID))
	})

	t.Run("none makes no repair", func(t *testing.T) {
		_, old := readyUnhealthy(t, "repair-none", healthCheck("none", 3))

		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), old))

		current := fixture.get(t, old.ID)
		assert.Equal(t, "ready", current.Status)
		assert.Equal(t, "running", current.Desired)
		assert.True(t, current.UnhealthySince.Valid, "the tools stay withdrawn")
		assert.Empty(t, current.Repairs)
		assert.Zero(t, fixture.healthStops(t, old.ID))
	})

	t.Run("reapply applies the objects again and counts again a whole detection later", func(t *testing.T) {
		template, old := readyUnhealthy(t, "repair-reapply", healthCheck("reapply", 3))
		template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
			`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"user":"${principal.id}"}}`)}}
		fixture.catalog.Set(template, nil)

		kube := repairKube(t)
		repairer := fixture.repairer(kube)
		start := *fixture.now.Load()

		defer fixture.setNow(time.Now())

		require.NoError(t, repairer.Step(t.Context(), old))

		configMap := &corev1.ConfigMap{}
		require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: old.Namespace, Name: "settings"}, configMap),
			"the deleted object is applied again")
		assert.Equal(t, "repair-reapply-user", configMap.Data["user"])

		current := fixture.get(t, old.ID)
		assert.Equal(t, "ready", current.Status, "the row keeps its ID")
		assert.Equal(t, "running", current.Desired)
		assert.True(t, current.UnhealthySince.Valid, "it stays unhealthy until the check passes")
		require.Len(t, current.Repairs, 1)
		assert.Equal(t, 1, fixture.auditCount(t, "instance.reapply", old.ID))
		assert.Zero(t, fixture.healthStops(t, old.ID))

		// Within failureThreshold × interval (30 s) no repair is counted.
		fixture.setNow(start.Add(29 * time.Second))
		require.NoError(t, repairer.Step(t.Context(), current))
		assert.Len(t, fixture.get(t, old.ID).Repairs, 1)

		// Still unhealthy 30 s later: one more repair.
		fixture.setNow(start.Add(30 * time.Second))
		require.NoError(t, repairer.Step(t.Context(), current))
		current = fixture.get(t, old.ID)
		assert.Len(t, current.Repairs, 2)
		assert.Equal(t, 2, fixture.auditCount(t, "instance.reapply", old.ID))

		// The third reapply is the last; the next one fails the row.
		fixture.setNow(start.Add(60 * time.Second))
		require.NoError(t, repairer.Step(t.Context(), current))
		current = fixture.get(t, old.ID)
		require.Len(t, current.Repairs, 3)

		fixture.setNow(start.Add(90 * time.Second))
		require.NoError(t, repairer.Step(t.Context(), current))
		current = fixture.get(t, old.ID)
		assert.Equal(t, "failed", current.Status)
		assert.Equal(t, "Health check still failing after 3 repairs in 30 minutes: "+unhealthyReason, current.Reason)
		assert.Equal(t, 3, fixture.auditCount(t, "instance.reapply", old.ID))
	})

	t.Run("a template without a health check is replaced", func(t *testing.T) {
		_, old := readyUnhealthy(t, "repair-default", v1alpha1.HealthCheck{})
		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET health_check = NULL WHERE id = $1", old.ID)
		require.NoError(t, err)

		require.NoError(t, fixture.repairer(repairKube(t)).Step(t.Context(), fixture.get(t, old.ID)))
		assert.Equal(t, 1, fixture.healthStops(t, old.ID))
	})
}
