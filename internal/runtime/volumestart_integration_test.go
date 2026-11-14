//go:build integration

package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const gib = int64(1) << 30

var retainPolicy = corev1.PersistentVolumeReclaimRetain

// volumeStartFixture is one PostgreSQL database, a fake Kubernetes API and
// a Starter with persistent volumes on. Subtests share the database and keep
// apart by using their own template UIDs and principals.
type volumeStartFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	store   runtime.PostgresStartStore
	counter atomic.Int64
}

func newVolumeStartFixture(t *testing.T) *volumeStartFixture {
	t.Helper()

	postgres := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	return &volumeStartFixture{pool: pool, queries: db.New(pool), store: runtime.PostgresStartStore{Pool: pool}}
}

// volumeStarter is a Starter over the fixture's database, a fake API holding
// objects, and a clock the test moves.
type volumeStarter struct {
	starter *runtime.Starter
	kube    client.Client
	metrics *runtime.Metrics
	now     time.Time
}

func (f *volumeStartFixture) starter(t *testing.T, template *v1alpha1.MCPServerTemplate, limit int64,
	objects ...client.Object,
) *volumeStarter {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	started := &volumeStarter{
		kube: clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
			WithInterceptorFuncs(interceptor.Funcs{Apply: reapplyFakeApply}).Build(),
		metrics: runtime.NewMetrics(),
		now:     time.Now(),
	}

	volumes := f.store
	volumes.Metrics = started.metrics

	started.starter = &runtime.Starter{
		Store:  volumes,
		Client: started.kube,
		Template: func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			if key != (types.NamespacedName{Namespace: template.Namespace, Name: template.Name}) {
				return nil, false
			}

			return template.DeepCopy(), true
		},
		Secrets: runtime.BrokerSecrets(func(context.Context, string, string) ([]byte, time.Time, error) {
			return nil, time.Time{}, errors.New("no secrets")
		}),
		ListTools: func(context.Context, string) ([]*mcp.Tool, string, error) {
			return []*mcp.Tool{{Name: "search", InputSchema: map[string]any{"type": "object"}}}, runtime.RevisionStateful, nil
		},
		GatewayNamespace:         "toolgate",
		GatewayPodLabels:         map[string]string{"app.kubernetes.io/name": "toolgate"},
		Volumes:                  volumes,
		PersistentVolumesEnabled: true,
		VolumeLimitPerUser:       limit,
		Now:                      func() time.Time { return started.now },
	}

	return started
}

// volumeTemplate is a per-user template with one volume per name.
func (f *volumeStartFixture) volumeTemplate(names ...string) *v1alpha1.MCPServerTemplate {
	number := f.counter.Add(1)
	template := testTemplate()
	template.Name = fmt.Sprintf("files-%d", number)
	template.UID = types.UID(fmt.Sprintf("uid-files-%d", number))

	for _, name := range names {
		template.Spec.PersistentVolumes = append(template.Spec.PersistentVolumes, v1alpha1.PersistentVolume{
			Name: name, Size: "1Gi", StorageClassName: "retain",
		})
	}

	return template
}

func (f *volumeStartFixture) principal() string {
	return fmt.Sprintf("user-%d", f.counter.Add(1))
}

// instance inserts a pending instance of the template for the principal.
func (f *volumeStartFixture) instance(t *testing.T, template *v1alpha1.MCPServerTemplate, principal string) db.Instance {
	t.Helper()

	number := f.counter.Add(1)
	id := fmt.Sprintf("%032x", number)

	instance, err := f.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		Generation: template.Generation, Scope: "per-user", ScopeKey: fmt.Sprintf("user:%s:%d", principal, number),
		OwnerPrincipal: principal, Namespace: runtime.NamespacePrefix + id,
	})
	require.NoError(t, err)

	return instance
}

func (f *volumeStartFixture) get(t *testing.T, id string) db.Instance {
	t.Helper()

	instance, err := f.queries.GetInstance(t.Context(), id)
	require.NoError(t, err)

	return instance
}

func (f *volumeStartFixture) setStatus(t *testing.T, id, status string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE instances SET status = $2 WHERE id = $1", id, status)
	require.NoError(t, err)
}

// stop moves an instance to stopped by the teardown's write, which also
// releases its volumes.
func (f *volumeStartFixture) stop(t *testing.T, id string) {
	t.Helper()

	f.setStatus(t, id, "stopping")
	changed, err := f.queries.SetInstanceStopped(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, int64(1), changed)
}

func (f *volumeStartFixture) volumeRows(t *testing.T, templateUID string) []db.PersistentVolume {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), "SELECT id FROM persistent_volumes WHERE template_uid = $1 ORDER BY volume_name, scope_id", templateUID)
	require.NoError(t, err)

	var ids []string

	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}

	require.NoError(t, rows.Err())

	result := make([]db.PersistentVolume, 0, len(ids))

	for _, id := range ids {
		row, err := f.queries.GetPersistentVolume(t.Context(), id)
		require.NoError(t, err)

		result = append(result, row)
	}

	return result
}

func (f *volumeStartFixture) auditCount(t *testing.T, kind, instanceID string) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = $1 AND details->>'instance' = $2", kind, instanceID).Scan(&count))

	return count
}

// claims are the template's volumes as the Starter claims them.
func claims(template *v1alpha1.MCPServerTemplate, scope string) []runtime.VolumeClaim {
	result := make([]runtime.VolumeClaim, 0, len(template.Spec.PersistentVolumes))

	for _, volume := range template.Spec.PersistentVolumes {
		size := resource.MustParse(volume.Size)
		result = append(result, runtime.VolumeClaim{
			Name: volume.Name, Scope: scope, StorageClass: volume.StorageClassName, SizeBytes: size.Value(),
		})
	}

	return result
}

func step(t *testing.T, started *volumeStarter, f *volumeStartFixture, id string) db.Instance {
	t.Helper()

	require.NoError(t, started.starter.Step(t.Context(), f.get(t, id)))

	return f.get(t, id)
}

func TestVolumeClaimIsExclusive(t *testing.T) {
	fixture := newVolumeStartFixture(t)

	t.Run("a replacement waits until the old instance is stopped", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))

		old := fixture.instance(t, template, principal)
		result, err := fixture.store.ClaimVolumes(t.Context(), old, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		fixture.setStatus(t, old.ID, "stopping")

		replacement := fixture.instance(t, template, principal)
		waiting := step(t, started, fixture, replacement.ID)
		assert.Equal(t, runtime.StatusPending, waiting.Status)
		assert.Equal(t, "Volume data is still held by instance "+old.ID+", which is stopping.", waiting.Reason)

		rows := fixture.volumeRows(t, string(template.UID))
		require.Len(t, rows, 1)
		assert.Equal(t, old.ID, rows[0].InstanceID.String, "the old instance keeps the volume while it stops")

		changed, err := fixture.queries.SetInstanceStopped(t.Context(), old.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), changed)

		starting := step(t, started, fixture, replacement.ID)
		assert.Equal(t, runtime.StatusStarting, starting.Status, starting.Reason)

		rows = fixture.volumeRows(t, string(template.UID))
		require.Len(t, rows, 1)
		assert.Equal(t, replacement.ID, rows[0].InstanceID.String)
	})

	t.Run("two claims at once leave one holder", func(t *testing.T) {
		template := fixture.volumeTemplate("cache", "data")
		principal := fixture.principal()
		first := fixture.instance(t, template, principal)
		second := fixture.instance(t, template, principal)

		var group sync.WaitGroup

		results := make([]runtime.VolumeClaimResult, 2)
		errs := make([]error, 2)

		for index, instance := range []db.Instance{first, second} {
			group.Add(1)

			go func() {
				defer group.Done()

				results[index], errs[index] = fixture.store.ClaimVolumes(t.Context(), instance, template.Name,
					claims(template, "per-user"), 0)
			}()
		}

		group.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])

		holders := 0

		for _, result := range results {
			if len(result.Rows) > 0 {
				holders++

				assert.Len(t, result.Rows, 2, "a claim takes every volume")
			} else {
				assert.NotEmpty(t, result.Wait, "the other claim waits")
			}
		}

		assert.Equal(t, 1, holders)

		rows := fixture.volumeRows(t, string(template.UID))
		require.Len(t, rows, 2)
		assert.Equal(t, rows[0].InstanceID, rows[1].InstanceID, "one instance holds every volume")
	})
}

func TestVolumeLimitPerUser(t *testing.T) {
	fixture := newVolumeStartFixture(t)

	// fill gives the principal a per-user volume of another template.
	fill := func(t *testing.T, principal string, size int64) {
		t.Helper()

		other := fixture.volumeTemplate()
		_, err := fixture.queries.InsertPersistentVolume(t.Context(), db.InsertPersistentVolumeParams{
			ID: fmt.Sprintf("%032x", fixture.counter.Add(1)), TemplateNs: other.Namespace, TemplateName: other.Name,
			TemplateUid: string(other.UID), VolumeName: "data", Scope: "per-user", ScopeID: principal,
			StorageClass: "retain", SizeBytes: size,
		})
		require.NoError(t, err)
	}

	t.Run("a new volume past the limit fails the start", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		fill(t, principal, gib/2+1)
		started := fixture.starter(t, template, gib+gib/2, storageClass("retain", &retainPolicy))

		instance := fixture.instance(t, template, principal)
		failed := step(t, started, fixture, instance.ID)

		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, template.Name+" needs a 1Gi volume data, but your volumes already use 536870913 of your 1536Mi limit. "+
			"Ask an admin to delete a volume you no longer need (toolgate admin volumes).", failed.Reason)
		assert.Equal(t, 1, fixture.auditCount(t, "instance.fail", instance.ID))
		assert.InDelta(t, 1, promtestutil.ToFloat64(started.metrics.PersistentVolumeEvents.WithLabelValues(runtime.VolumeEventLimitRefused)), 0)
		assert.Empty(t, fixture.volumeRows(t, string(template.UID)), "the refused volume gets no row")
	})

	t.Run("a new volume that reaches the limit exactly starts", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		fill(t, principal, gib/2)
		started := fixture.starter(t, template, gib+gib/2, storageClass("retain", &retainPolicy))

		instance := fixture.instance(t, template, principal)
		starting := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusStarting, starting.Status, starting.Reason)
	})

	t.Run("an existing volume binds over the limit", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))

		first := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusStarting, step(t, started, fixture, first.ID).Status)
		fixture.stop(t, first.ID)

		fill(t, principal, 5*gib)
		started.starter.VolumeLimitPerUser = gib

		second := fixture.instance(t, template, principal)
		starting := step(t, started, fixture, second.ID)
		assert.Equal(t, runtime.StatusStarting, starting.Status, starting.Reason)
	})

	t.Run("two new volumes near the limit start only the one that fits", func(t *testing.T) {
		principal := fixture.principal()
		templates := []*v1alpha1.MCPServerTemplate{fixture.volumeTemplate("data"), fixture.volumeTemplate("data")}
		instances := []db.Instance{fixture.instance(t, templates[0], principal), fixture.instance(t, templates[1], principal)}

		var group sync.WaitGroup

		results := make([]runtime.VolumeClaimResult, 2)
		errs := make([]error, 2)

		for index := range instances {
			group.Add(1)

			go func() {
				defer group.Done()

				results[index], errs[index] = fixture.store.ClaimVolumes(t.Context(), instances[index], templates[index].Name,
					claims(templates[index], "per-user"), gib+gib/2)
			}()
		}

		group.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])

		assert.NotEqual(t, results[0].Refused, results[1].Refused, "exactly one start is refused")

		for index, result := range results {
			if result.Refused {
				assert.Equal(t, "failed", fixture.get(t, instances[index].ID).Status)
			} else {
				assert.Len(t, result.Rows, 1)
			}
		}
	})
}

func TestVolumeTemplateChanges(t *testing.T) {
	fixture := newVolumeStartFixture(t)

	t.Run("size and class changes keep the row's values", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy), storageClass("fast", &retainPolicy))

		first := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusStarting, step(t, started, fixture, first.ID).Status)
		fixture.stop(t, first.ID)

		template.Spec.PersistentVolumes[0].Size = "2Gi"
		template.Spec.PersistentVolumes[0].StorageClassName = "fast"

		second := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusStarting, step(t, started, fixture, second.ID).Status)

		rows := fixture.volumeRows(t, string(template.UID))
		require.Len(t, rows, 1)
		assert.Equal(t, gib, rows[0].SizeBytes)
		assert.Equal(t, "retain", rows[0].StorageClass)

		var claim corev1.PersistentVolumeClaim
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Namespace: second.Namespace, Name: "data"}, &claim))
		assert.Equal(t, gib, claim.Spec.Resources.Requests.Storage().Value())
		assert.Equal(t, "retain", *claim.Spec.StorageClassName)
	})

	t.Run("a scope change uses rows of the new scope ID", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()

		first := fixture.instance(t, template, principal)
		_, err := fixture.store.ClaimVolumes(t.Context(), first, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		fixture.stop(t, first.ID)

		second := fixture.instance(t, template, principal)
		result, err := fixture.store.ClaimVolumes(t.Context(), second, template.Name, claims(template, "per-template"), 0)
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		assert.Empty(t, result.Rows[0].ScopeID)

		rows := fixture.volumeRows(t, string(template.UID))
		require.Len(t, rows, 2, "the per-user row stays, unheld")
		assert.False(t, rows[1].InstanceID.Valid)
		assert.Equal(t, principal, rows[1].ScopeID)
	})

	t.Run("a removed or renamed volume is released on stop", func(t *testing.T) {
		template := fixture.volumeTemplate("cache", "data")
		principal := fixture.principal()

		first := fixture.instance(t, template, principal)
		_, err := fixture.store.ClaimVolumes(t.Context(), first, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)

		template.Spec.PersistentVolumes = template.Spec.PersistentVolumes[1:]
		template.Spec.PersistentVolumes[0].Name = "state"

		held, err := fixture.store.HeldVolumes(t.Context(), first.ID)
		require.NoError(t, err)
		assert.Len(t, held, 2, "the running instance keeps its claims")

		fixture.stop(t, first.ID)

		second := fixture.instance(t, template, principal)
		result, err := fixture.store.ClaimVolumes(t.Context(), second, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		assert.Equal(t, "state", result.Rows[0].VolumeName)

		for _, row := range fixture.volumeRows(t, string(template.UID)) {
			if row.VolumeName != "state" {
				assert.False(t, row.InstanceID.Valid, "volume %s is released", row.VolumeName)
			}
		}
	})

	t.Run("a re-created template gets new rows", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()

		first := fixture.instance(t, template, principal)
		before, err := fixture.store.ClaimVolumes(t.Context(), first, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		fixture.stop(t, first.ID)

		template.UID = types.UID(string(template.UID) + "-again")

		second := fixture.instance(t, template, principal)
		after, err := fixture.store.ClaimVolumes(t.Context(), second, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		require.Len(t, after.Rows, 1)
		assert.NotEqual(t, before.Rows[0].ID, after.Rows[0].ID)
		assert.False(t, after.Rows[0].PvName.Valid, "the new row starts empty")
	})
}

// recordedVolume claims the template's volume for a stopped instance and
// records pvName as its PV, as an earlier instance would have.
func (f *volumeStartFixture) recordedVolume(t *testing.T, template *v1alpha1.MCPServerTemplate, principal, pvName string) db.PersistentVolume {
	t.Helper()

	earlier := f.instance(t, template, principal)
	result, err := f.store.ClaimVolumes(t.Context(), earlier, template.Name, claims(template, "per-user"), 0)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)

	recorded, err := f.store.RecordVolume(t.Context(), earlier, result.Rows[0], pvName, gib)
	require.NoError(t, err)
	require.True(t, recorded)
	f.stop(t, earlier.ID)

	row, err := f.queries.GetPersistentVolume(t.Context(), result.Rows[0].ID)
	require.NoError(t, err)

	return row
}

func TestVolumeStartWaitsAndFailures(t *testing.T) {
	fixture := newVolumeStartFixture(t)

	t.Run("a PV bound to an old claim waits, then times out", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		row := fixture.recordedVolume(t, template, principal, "pv-old")
		pv := ownedPV(row, corev1.VolumeBound, &corev1.ObjectReference{Namespace: "tg-old", Name: "data"})
		pv.Name = "pv-old"
		started := fixture.starter(t, template, 0, pv)

		instance := fixture.instance(t, template, principal)
		waiting := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusPending, waiting.Status)
		assert.Equal(t, "Volume data is still bound to tg-old/data.", waiting.Reason)

		started.now = started.now.Add(6 * time.Minute)
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "Timed out waiting for volume data: Volume data is still bound to tg-old/data.", failed.Reason)
	})

	t.Run("a PV with the Delete policy fails the start", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		row := fixture.recordedVolume(t, template, principal, "pv-delete")
		pv := ownedPV(row, corev1.VolumeReleased, nil)
		pv.Name = "pv-delete"
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		started := fixture.starter(t, template, 0, pv)

		instance := fixture.instance(t, template, principal)
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "PersistentVolume pv-delete has reclaim policy Delete, so toolgate won't bind it.", failed.Reason)

		row, err := fixture.queries.GetPersistentVolume(t.Context(), row.ID)
		require.NoError(t, err)
		assert.Equal(t, failed.Reason, row.Problem)
	})

	t.Run("a missing StorageClass fails the start with the class named", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		started := fixture.starter(t, template, 0)

		instance := fixture.instance(t, template, fixture.principal())
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "StorageClass retain for volume data doesn't exist.", failed.Reason)
	})

	t.Run("an old instance stuck stopping times the start out", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))

		old := fixture.instance(t, template, principal)
		_, err := fixture.store.ClaimVolumes(t.Context(), old, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		fixture.setStatus(t, old.ID, "stopping")

		instance := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusPending, step(t, started, fixture, instance.ID).Status)

		started.now = started.now.Add(6 * time.Minute)
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "Timed out waiting for volume data: Volume data is still held by instance "+old.ID+
			", which is stopping.", failed.Reason)
	})

	t.Run("a start with the feature off fails at once", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))
		started.starter.PersistentVolumesEnabled = false

		instance := fixture.instance(t, template, fixture.principal())
		failed := step(t, started, fixture, instance.ID)
		assert.Equal(t, "failed", failed.Status)
		assert.Equal(t, "persistentVolumes needs the gateway setting persistentVolumes.enabled", failed.Reason)
		assert.Empty(t, fixture.volumeRows(t, string(template.UID)), "nothing is claimed")
	})

	t.Run("a lost PV is cleared and the volume starts empty", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		row := fixture.recordedVolume(t, template, principal, "pv-gone")
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))

		instance := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusStarting, step(t, started, fixture, instance.ID).Status)

		row, err := fixture.queries.GetPersistentVolume(t.Context(), row.ID)
		require.NoError(t, err)
		assert.False(t, row.PvName.Valid)
		assert.Equal(t, 1, fixture.auditCount(t, "volume.lost", instance.ID))
		assert.InDelta(t, 1, promtestutil.ToFloat64(started.metrics.PersistentVolumeEvents.WithLabelValues(runtime.VolumeEventLost)), 0)

		var claim corev1.PersistentVolumeClaim
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Namespace: instance.Namespace, Name: "data"}, &claim))
		assert.Empty(t, claim.Spec.VolumeName, "the provisioner makes a new PV")
	})

	t.Run("a released PV is pre-bound and its claim names it", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		row := fixture.recordedVolume(t, template, principal, "pv-free")
		pv := ownedPV(row, corev1.VolumeReleased, &corev1.ObjectReference{Namespace: "tg-old", Name: "data", UID: "old"})
		pv.Name = "pv-free"
		started := fixture.starter(t, template, 0, pv)

		instance := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusStarting, step(t, started, fixture, instance.ID).Status)
		assert.InDelta(t, 1, promtestutil.ToFloat64(started.metrics.PersistentVolumeEvents.WithLabelValues(runtime.VolumeEventRebound)), 0)

		var bound corev1.PersistentVolume
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Name: "pv-free"}, &bound))
		require.NotNil(t, bound.Spec.ClaimRef)
		assert.Equal(t, instance.Namespace, bound.Spec.ClaimRef.Namespace)
		assert.Empty(t, bound.Spec.ClaimRef.UID)

		var claim corev1.PersistentVolumeClaim
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Namespace: instance.Namespace, Name: "data"}, &claim))
		assert.Equal(t, "pv-free", claim.Spec.VolumeName)
	})

	t.Run("a new volume's PV is recorded before the instance is ready", func(t *testing.T) {
		template := fixture.volumeTemplate("data")
		principal := fixture.principal()
		started := fixture.starter(t, template, 0, storageClass("retain", &retainPolicy))

		instance := fixture.instance(t, template, principal)
		assert.Equal(t, runtime.StatusStarting, step(t, started, fixture, instance.ID).Status)

		notBound := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusStarting, notBound.Status, "an unbound claim keeps the instance starting")

		// The claim binds first. The provisioner's PV named after the claim's
		// UID is not readable yet.
		key := types.NamespacedName{Namespace: instance.Namespace, Name: "data"}

		var claim corev1.PersistentVolumeClaim
		require.NoError(t, started.kube.Get(t.Context(), key, &claim))
		claim.UID = "claim-uid"
		require.NoError(t, started.kube.Update(t.Context(), &claim))
		require.NoError(t, started.kube.Get(t.Context(), key, &claim))
		pvName := "pvc-" + string(claim.UID)

		claim.Spec.VolumeName = pvName
		require.NoError(t, started.kube.Update(t.Context(), &claim))
		claim.Status.Phase = corev1.ClaimBound
		require.NoError(t, started.kube.Status().Update(t.Context(), &claim))

		notRecorded := step(t, started, fixture, instance.ID)
		assert.Equal(t, runtime.StatusStarting, notRecorded.Status, "a bound claim whose PV isn't recorded keeps the instance starting")

		// The PV appears, and the next step records it.
		require.NoError(t, started.kube.Create(t.Context(), &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: pvName, Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "x"},
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				ClaimRef:                      &corev1.ObjectReference{Namespace: instance.Namespace, Name: "data", UID: claim.UID},
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			},
		}))

		ready := step(t, started, fixture, instance.ID)
		assert.Equal(t, "ready", ready.Status, ready.Reason)

		rows := fixture.volumeRows(t, string(template.UID))
		require.Len(t, rows, 1)
		assert.Equal(t, pgtype.Text{String: pvName, Valid: true}, rows[0].PvName)
		assert.Equal(t, 2*gib, rows[0].SizeBytes)
		assert.Equal(t, 1, fixture.auditCount(t, "volume.create", instance.ID))
		assert.InDelta(t, 1, promtestutil.ToFloat64(started.metrics.PersistentVolumeEvents.WithLabelValues(runtime.VolumeEventCreated)), 0)
	})
}
