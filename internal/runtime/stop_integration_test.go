//go:build integration

package runtime_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const templateNamespace = "catalog"

// stopFixture is one PostgreSQL database, a catalog, a fake Kubernetes API
// and the reconciler's full pass over them. Subtests share it and keep apart
// by using their own template names and principals.
type stopFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	catalog *catalog.Catalog
	kube    client.Client
	metrics *runtime.Metrics
	logs    *bytes.Buffer
	now     atomic.Pointer[time.Time]
	counter atomic.Int64
}

func newStopFixture(t *testing.T) *stopFixture {
	t.Helper()

	postgres := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	templates := catalog.New()
	templates.MarkLoaded()

	fixture := &stopFixture{
		pool:    pool,
		queries: db.New(pool),
		catalog: templates,
		kube:    clientfake.NewClientBuilder().Build(),
		metrics: runtime.NewMetrics(),
		logs:    &bytes.Buffer{},
	}
	fixture.setNow(time.Now())

	return fixture
}

func (f *stopFixture) setNow(now time.Time) { f.now.Store(&now) }

// pass runs one full pass of the reconciler, holding the Lease.
func (f *stopFixture) pass(t *testing.T) {
	t.Helper()

	fence := &fakeFence{}
	fence.holds.Store(true)

	now := func() time.Time { return *f.now.Load() }
	logger := slog.New(slog.NewTextHandler(f.logs, nil))
	reconciler := &runtime.Reconciler{Fence: fence, Client: f.kube}
	stops := &runtime.StopDecider{Pool: f.pool, Templates: f.catalog, Log: logger, Now: now}
	teardown := &runtime.Teardown{Pool: f.pool, Reader: f.kube, Deleter: reconciler, Log: logger, Metrics: f.metrics, Now: now}

	pass := teardown.Pass(f.queries, runtime.FullPass(f.queries, logger, stops.Step, teardown.Step))
	require.NoError(t, pass(t.Context()))
}

// template adds a valid template to the catalog and returns it.
func (f *stopFixture) template(name, scope string, idleStop time.Duration, access v1alpha1.Access) *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: name, Generation: 1, UID: types.UID("uid-" + name)},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:    scope,
			IdleStop: &metav1.Duration{Duration: idleStop},
			Access:   access,
		},
	}
	f.catalog.Set(template, nil)

	return template
}

// session creates a session with a user's identity snapshot.
func (f *stopFixture) session(t *testing.T, principal, sessionID, email string, groups ...string) db.Session {
	t.Helper()

	return f.sessionOf(t, principal, sessionID, "claimed/default", "user", "user", email, groups...)
}

// sessionOf creates a session with an identity snapshot of the given
// workspace, attestation and kind.
func (f *stopFixture) sessionOf(t *testing.T, principal, sessionID, workspace, attestation, kind, email string, groups ...string) db.Session {
	t.Helper()

	session, err := f.queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: principal, SessionID: sessionID, Workspace: workspace, Attestation: attestation, Kind: kind,
		Email: email, Groups: append([]string{}, groups...),
	})
	require.NoError(t, err)

	return session
}

func (f *stopFixture) enable(t *testing.T, session db.Session, template *v1alpha1.MCPServerTemplate) {
	t.Helper()
	require.NoError(t, f.queries.EnableServer(t.Context(), db.EnableServerParams{
		SessionPk: session.ID, TemplateNs: template.Namespace, TemplateName: template.Name,
	}))
}

// instance inserts an instance row in the given status and creates its
// labelled namespace.
func (f *stopFixture) instance(t *testing.T, template *v1alpha1.MCPServerTemplate, scopeKey, owner, status string) db.Instance {
	t.Helper()

	id := fmt.Sprintf("%032x", f.counter.Add(1))
	instance, err := f.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		Generation: template.Generation, Scope: template.Spec.Scope, ScopeKey: scopeKey, OwnerPrincipal: owner,
		Namespace: "tg-" + id,
	})
	require.NoError(t, err)

	if status != "pending" {
		_, err = f.queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: id, Generation: template.Generation})
		require.NoError(t, err)
	}

	switch status {
	case "ready":
		_, err = f.queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: id, Tools: []byte(`[]`)})
		require.NoError(t, err)
	case "failed":
		_, err = f.queries.SetInstanceFailed(t.Context(), db.SetInstanceFailedParams{ID: id, Reason: "boom", FromStatus: "starting"})
		require.NoError(t, err)
	}

	require.NoError(t, f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Namespace, Labels: map[string]string{runtime.LabelInstance: id, runtime.LabelManagedBy: runtime.ManagedByValue},
	}}))

	return f.get(t, id)
}

// sharedInstance inserts a failed instance row in the shared namespace,
// creating the namespace once, and gives it a labelled ConfigMap and
// Service. It returns the row.
func (f *stopFixture) sharedInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, status string) db.Instance {
	t.Helper()

	err := f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sharedNamespace, Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
	}})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}

	id := fmt.Sprintf("%032x", f.counter.Add(1))
	_, err = f.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		Generation: template.Generation, Scope: template.Spec.Scope, ScopeKey: "shared", Namespace: sharedNamespace,
	})
	require.NoError(t, err)

	_, err = f.queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: id, Generation: template.Generation})
	require.NoError(t, err)

	switch status {
	case "ready":
		_, err = f.queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: id, Tools: []byte(`[]`)})
	case "failed":
		_, err = f.queries.SetInstanceFailed(t.Context(), db.SetInstanceFailedParams{ID: id, Reason: "boom", FromStatus: "starting"})
	}

	require.NoError(t, err)

	f.createSharedObjects(t, id)

	return f.get(t, id)
}

// createSharedObjects creates a ConfigMap and a Service labelled for the
// instance id in the shared namespace.
func (f *stopFixture) createSharedObjects(t *testing.T, id string) {
	t.Helper()

	labels := map[string]string{runtime.LabelInstance: id, runtime.LabelManagedBy: runtime.ManagedByValue}
	require.NoError(t, f.kube.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: sharedNamespace, Name: "settings-" + id, Labels: labels,
	}}))
	require.NoError(t, f.kube.Create(t.Context(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: sharedNamespace, Name: "server-" + id, Labels: labels,
	}}))
}

// sharedObjectNames returns the names of the ConfigMaps and Services in the
// shared namespace.
func (f *stopFixture) sharedObjectNames(t *testing.T) []string {
	t.Helper()

	var names []string

	configMaps := &corev1.ConfigMapList{}
	require.NoError(t, f.kube.List(t.Context(), configMaps, client.InNamespace(sharedNamespace)))

	for _, configMap := range configMaps.Items {
		names = append(names, "ConfigMap/"+configMap.Name)
	}

	services := &corev1.ServiceList{}
	require.NoError(t, f.kube.List(t.Context(), services, client.InNamespace(sharedNamespace)))

	for _, service := range services.Items {
		names = append(names, "Service/"+service.Name)
	}

	return names
}

func (f *stopFixture) get(t *testing.T, id string) db.Instance {
	t.Helper()

	instance, err := f.queries.GetInstance(t.Context(), id)
	require.NoError(t, err)

	return instance
}

// requireRunning checks that the instance is still live and its namespace
// is untouched.
func (f *stopFixture) requireRunning(t *testing.T, instance db.Instance) {
	t.Helper()

	current := f.get(t, instance.ID)
	require.Equal(t, "running", current.Desired)
	require.Equal(t, instance.Status, current.Status)

	var namespace corev1.Namespace
	require.NoError(t, f.kube.Get(t.Context(), types.NamespacedName{Name: instance.Namespace}, &namespace))
	require.Nil(t, namespace.DeletionTimestamp)
}

// requireTornDown checks that the instance is to be stopped, goes to
// stopping with its namespace deleted on the next pass, and to stopped on the
// pass after.
func (f *stopFixture) requireTornDown(t *testing.T, instance db.Instance) {
	t.Helper()

	require.Equal(t, "stopped", f.get(t, instance.ID).Desired)

	f.pass(t)

	current := f.get(t, instance.ID)
	require.Equal(t, "stopping", current.Status)

	err := f.kube.Get(t.Context(), types.NamespacedName{Name: instance.Namespace}, &corev1.Namespace{})
	require.True(t, apierrors.IsNotFound(err), "the namespace should be deleted, got %v", err)

	f.pass(t)
	require.Equal(t, "stopped", f.get(t, instance.ID).Status)
}

func TestStopDecisions(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}}

	t.Run("[row 10] a deleted template stops its instance", func(t *testing.T) {
		template := fixture.template("deleted", "shared", 0, everyone)
		session := fixture.session(t, "deleted-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "shared", "", "ready")

		fixture.pass(t)
		fixture.requireRunning(t, instance)

		fixture.catalog.Delete(types.NamespacedName{Namespace: templateNamespace, Name: template.Name})
		fixture.pass(t)
		fixture.requireTornDown(t, instance)
	})

	t.Run("a template deleted and created again stops the old instance", func(t *testing.T) {
		template := fixture.template("recreated", "shared", 0, everyone)
		session := fixture.session(t, "recreated-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "shared", "", "ready")

		recreated := template.DeepCopy()
		recreated.UID = "uid-recreated-again"
		fixture.catalog.Set(recreated, nil)
		fixture.pass(t)
		fixture.requireTornDown(t, instance)
	})

	t.Run("[row 29] a scope change stops the instance and keeps the session's enable", func(t *testing.T) {
		template := fixture.template("rescoped", "per-user", time.Hour, everyone)
		session := fixture.session(t, "rescoped-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "user:rescoped-user", "rescoped-user", "ready")

		fixture.pass(t)
		fixture.requireRunning(t, instance)

		rescoped := template.DeepCopy()
		rescoped.Spec.Scope = "shared"
		rescoped.Generation = 2
		fixture.catalog.Set(rescoped, nil)
		fixture.pass(t)
		fixture.requireTornDown(t, instance)

		enabled, err := fixture.queries.ListSessionServers(t.Context(), session.ID)
		require.NoError(t, err)
		require.Len(t, enabled, 1)
	})

	t.Run("[row 11] access removed in the owner's latest session snapshot stops the instance", func(t *testing.T) {
		template := fixture.template("access", "per-user", time.Hour, everyone)
		session := fixture.session(t, "access-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "user:access-user", "access-user", "ready")

		fixture.pass(t)
		fixture.requireRunning(t, instance)

		// A later request on another session no longer has the group.
		fixture.session(t, "access-user", "s2", "")
		fixture.pass(t)
		fixture.requireTornDown(t, instance)
	})

	t.Run("an attested user's snapshot is judged as a user whatever its principal looks like", func(t *testing.T) {
		const principal = "system:serviceaccount:team:attested-user"

		userAccess := fixture.template("kind-users", "per-user", time.Hour, v1alpha1.Access{Users: []string{principal}})
		serviceAccountAccess := fixture.template("kind-service-accounts", "per-user", time.Hour, v1alpha1.Access{ServiceAccounts: []string{principal}})
		session := fixture.sessionOf(t, principal, "s1", "coder/alice/w", "attested", "user", "")
		fixture.enable(t, session, userAccess)
		fixture.enable(t, session, serviceAccountAccess)
		kept := fixture.instance(t, userAccess, "user:"+principal, principal, "ready")
		stopped := fixture.instance(t, serviceAccountAccess, "user:"+principal, principal, "ready")

		fixture.pass(t)
		fixture.requireRunning(t, kept)
		fixture.requireTornDown(t, stopped)
	})

	t.Run("[row 13] a per-session instance stops once its session has ended, and the enabled rows stay", func(t *testing.T) {
		template := fixture.template("per-session", "per-session", 0, everyone)
		session := fixture.session(t, "session-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		instance := fixture.instance(t, template, "session:session-user:s1", "session-user", "starting")

		// Another session of the same user doesn't keep it running.
		other := fixture.session(t, "session-user", "s2", "", "staff")
		fixture.enable(t, other, template)

		fixture.pass(t)
		fixture.requireRunning(t, instance)

		// Ending the session keeps its enabled rows; the session isn't live.
		_, err := fixture.pool.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE id = $1", session.ID)
		require.NoError(t, err)
		fixture.pass(t)
		fixture.requireTornDown(t, instance)

		enabled, err := fixture.queries.ListSessionServers(t.Context(), session.ID)
		require.NoError(t, err)
		require.Len(t, enabled, 1, "ending a session keeps its enabled rows")
	})

	t.Run("a per-user instance stops once no session of its owner enables it and it is idle", func(t *testing.T) {
		template := fixture.template("per-user-idle", "per-user", 10*time.Minute, everyone)
		session := fixture.session(t, "idle-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		// Another user's session doesn't keep it running.
		fixture.enable(t, fixture.session(t, "idle-other", "s1", "", "staff"), template)
		instance := fixture.instance(t, template, "user:idle-user", "idle-user", "ready")

		lastUsed := time.Now().Add(-time.Hour).Truncate(time.Second)
		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET last_used_at = $1 WHERE id = $2", lastUsed, instance.ID)
		require.NoError(t, err)

		fixture.setNow(lastUsed.Add(time.Hour))
		fixture.pass(t)
		fixture.requireRunning(t, instance)

		require.NoError(t, fixture.queries.DeleteSessionServers(t.Context(), session.ID))

		fixture.setNow(lastUsed.Add(10 * time.Minute))
		fixture.pass(t)
		fixture.requireRunning(t, instance)

		fixture.setNow(lastUsed.Add(10*time.Minute + time.Second))
		fixture.pass(t)
		fixture.requireTornDown(t, instance)
	})

	t.Run("a shared instance stops when idle unless idleStop is 0", func(t *testing.T) {
		forever := fixture.template("shared-forever", "shared", 0, everyone)
		idle := fixture.template("shared-idle", "shared", 10*time.Minute, everyone)
		keeps := fixture.instance(t, forever, "shared", "", "ready")
		stops := fixture.instance(t, idle, "shared", "", "ready")

		// Never used: ready_at counts.
		fixture.setNow(stops.ReadyAt.Time.Add(10 * time.Minute))
		fixture.pass(t)
		fixture.requireRunning(t, stops)

		fixture.setNow(stops.ReadyAt.Time.Add(10*time.Minute + time.Second))
		fixture.pass(t)
		fixture.requireRunning(t, keeps)
		fixture.requireTornDown(t, stops)
	})

	t.Run("[row 23] a stop re-checks under the lock and sees an enable that committed first", func(t *testing.T) {
		template := fixture.template("race", "per-session", 0, everyone)
		session := fixture.session(t, "race-user", "s1", "", "staff")
		instance := fixture.instance(t, template, "session:race-user:s1", "race-user", "ready")

		// An enable holds the advisory lock and hasn't committed yet.
		enable, err := fixture.pool.Begin(t.Context())
		require.NoError(t, err)

		defer func() { _ = enable.Rollback(context.WithoutCancel(t.Context())) }()

		enableQueries := db.New(enable)
		require.NoError(t, enableQueries.LockInstanceScope(t.Context(), db.LockInstanceScopeParams{
			TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: instance.ScopeKey,
		}))
		require.NoError(t, enableQueries.EnableServer(t.Context(), db.EnableServerParams{
			SessionPk: session.ID, TemplateNs: template.Namespace, TemplateName: template.Name,
		}))

		decided := make(chan error, 1)
		stops := &runtime.StopDecider{Pool: fixture.pool, Templates: fixture.catalog, Log: slog.New(slog.DiscardHandler)}

		go func() {
			_, err := stops.Decide(context.WithoutCancel(t.Context()), instance)
			decided <- err
		}()

		require.Eventually(t, func() bool {
			var waiting int

			err := fixture.pool.QueryRow(t.Context(),
				"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted").Scan(&waiting)

			return err == nil && waiting == 1
		}, 10*time.Second, 10*time.Millisecond, "the stop decision should wait for the advisory lock")

		require.NoError(t, enable.Commit(t.Context()))
		require.NoError(t, <-decided)
		fixture.requireRunning(t, instance)
	})

	t.Run("[row 23] when the stop commits first the instance isn't live for the enable", func(t *testing.T) {
		template := fixture.template("race-stop-first", "per-session", 0, everyone)
		instance := fixture.instance(t, template, "session:race-stop-user:s1", "race-stop-user", "ready")

		stops := &runtime.StopDecider{Pool: fixture.pool, Templates: fixture.catalog, Log: slog.New(slog.DiscardHandler)}
		_, err := stops.Decide(t.Context(), instance)
		require.NoError(t, err)

		_, err = fixture.queries.GetLiveInstance(t.Context(), db.GetLiveInstanceParams{
			TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: instance.ScopeKey,
		})
		require.ErrorIs(t, err, pgx.ErrNoRows)
	})
}

func TestTeardown(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}}

	t.Run("a failed instance is torn down", func(t *testing.T) {
		template := fixture.template("failed", "shared", 0, everyone)
		instance := fixture.instance(t, template, "shared", "", "failed")

		fixture.requireTornDown(t, instance)
		assert.Equal(t, "boom", fixture.get(t, instance.ID).Reason)
	})

	t.Run("a namespace not labelled for the instance isn't deleted", func(t *testing.T) {
		template := fixture.template("shared-namespace", "shared", 0, everyone)
		instance := fixture.instance(t, template, "shared", "", "failed")

		var namespace corev1.Namespace
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Name: instance.Namespace}, &namespace))
		namespace.Labels = map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue}
		require.NoError(t, fixture.kube.Update(t.Context(), &namespace))

		fixture.pass(t)
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Name: instance.Namespace}, &namespace))
		require.Nil(t, namespace.DeletionTimestamp)
		current := fixture.get(t, instance.ID)
		require.Equal(t, "stopping", current.Status)
		require.Contains(t, current.Reason, "isn't deleted")
	})

	t.Run("an instance in the shared namespace is torn down by its labelled objects", func(t *testing.T) {
		template := fixture.template("shared-objects", "shared", 0, everyone)
		instance := fixture.sharedInstance(t, template, "failed")
		neighbour := fixture.sharedInstance(t, fixture.template("shared-neighbour", "shared", 0, everyone), "ready")

		// A labelled object of the instance that isn't toolgate's stays.
		require.NoError(t, fixture.kube.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: sharedNamespace, Name: "foreign", Labels: map[string]string{runtime.LabelInstance: instance.ID},
		}}))

		fixture.pass(t)
		require.Equal(t, "stopping", fixture.get(t, instance.ID).Status)
		assert.ElementsMatch(t, []string{
			"ConfigMap/foreign", "ConfigMap/settings-" + neighbour.ID, "Service/server-" + neighbour.ID,
		}, fixture.sharedObjectNames(t), "only the instance's own objects are deleted")
		require.True(t, fixture.namespaceExists(t, sharedNamespace), "the shared namespace stays")

		fixture.pass(t)
		require.Equal(t, "stopped", fixture.get(t, instance.ID).Status)
		require.True(t, fixture.namespaceExists(t, sharedNamespace), "the shared namespace stays")
		fixture.requireRunning(t, neighbour)

		err := fixture.kube.Get(t.Context(), types.NamespacedName{Name: "tg-" + instance.ID}, &corev1.Namespace{})
		require.True(t, apierrors.IsNotFound(err), "no tg-<id> namespace is involved, got %v", err)
	})

	t.Run("a namespace Terminating for more than 10 minutes is reported", func(t *testing.T) {
		template := fixture.template("stuck", "shared", 0, everyone)
		instance := fixture.instance(t, template, "shared", "", "failed")

		var namespace corev1.Namespace
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Name: instance.Namespace}, &namespace))
		namespace.Finalizers = []string{"example.com/hold"}
		require.NoError(t, fixture.kube.Update(t.Context(), &namespace))
		namespace.Status.Conditions = []corev1.NamespaceCondition{{
			Type: corev1.NamespaceFinalizersRemaining, Status: corev1.ConditionTrue, Message: "example.com/hold in 1 resource instances",
		}}
		require.NoError(t, fixture.kube.Status().Update(t.Context(), &namespace))

		fixture.pass(t)
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Name: instance.Namespace}, &namespace))
		require.NotNil(t, namespace.DeletionTimestamp)
		deleted := namespace.DeletionTimestamp.Time

		fixture.setNow(deleted.Add(runtime.StuckAfter))
		fixture.pass(t)
		assert.Equal(t, "boom", fixture.get(t, instance.ID).Reason)
		assert.InDelta(t, 0, promtestutil.ToFloat64(fixture.metrics.NamespacesStuck), 0)
		assert.NotContains(t, fixture.logs.String(), "still terminating")

		fixture.setNow(deleted.Add(runtime.StuckAfter + time.Second))
		fixture.pass(t)
		current := fixture.get(t, instance.ID)
		assert.Equal(t, "stopping", current.Status)
		assert.Contains(t, current.Reason, "example.com/hold in 1 resource instances")
		assert.InDelta(t, 1, promtestutil.ToFloat64(fixture.metrics.NamespacesStuck), 0)
		assert.Contains(t, fixture.logs.String(), "level=WARN msg=\"instance namespace is still terminating\"")
	})

	t.Run("stopped rows are deleted after 7 days", func(t *testing.T) {
		template := fixture.template("retention", "shared", 0, everyone)
		instance := fixture.instance(t, template, "shared", "", "failed")
		require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Namespace}}))

		fixture.setNow(time.Now())
		fixture.pass(t)
		stopped := fixture.get(t, instance.ID)
		require.Equal(t, "stopped", stopped.Status)

		fixture.setNow(stopped.UpdatedAt.Time.Add(runtime.StoppedRetention))
		fixture.pass(t)
		fixture.get(t, instance.ID)

		fixture.setNow(stopped.UpdatedAt.Time.Add(runtime.StoppedRetention + time.Second))
		fixture.pass(t)
		_, err := fixture.queries.GetInstance(t.Context(), instance.ID)
		require.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("toolgate_instances counts instances by scope and status", func(t *testing.T) {
		template := fixture.template("counted", "per-user", time.Hour, everyone)
		session := fixture.session(t, "counted-user", "s1", "", "staff")
		fixture.enable(t, session, template)
		fixture.instance(t, template, "user:counted-user", "counted-user", "ready")
		fixture.instance(t, fixture.template("counted-2", "per-user", time.Hour, everyone), "user:counted-user", "counted-user", "pending")
		fixture.enable(t, session, fixture.template("counted-2", "per-user", time.Hour, everyone))

		fixture.setNow(time.Now())
		fixture.pass(t)
		assert.InDelta(t, 1, promtestutil.ToFloat64(fixture.metrics.Instances.WithLabelValues("per-user", "ready")), 0)
		assert.InDelta(t, 1, promtestutil.ToFloat64(fixture.metrics.Instances.WithLabelValues("per-user", "pending")), 0)
	})
}

func TestStopDeciderDecidesNothingBeforeTheCatalogLoaded(t *testing.T) {
	fixture := newStopFixture(t)

	// The template is in the database but not read into this catalog yet.
	template := fixture.template("not-read-yet", "shared", 0, v1alpha1.Access{Groups: []string{"staff"}})
	instance := fixture.instance(t, template, "shared", "", "ready")

	unloaded := catalog.New()
	stops := &runtime.StopDecider{Pool: fixture.pool, Templates: unloaded, Log: slog.New(slog.DiscardHandler)}

	decided, err := stops.Decide(t.Context(), instance)
	require.NoError(t, err)
	assert.Equal(t, "running", decided.Desired)
	fixture.requireRunning(t, instance)

	// Once loaded, the same catalog without the template stops it.
	unloaded.MarkLoaded()

	decided, err = stops.Decide(t.Context(), instance)
	require.NoError(t, err)
	assert.Equal(t, "stopped", decided.Desired)
}
