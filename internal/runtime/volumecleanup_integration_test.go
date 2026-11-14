//go:build integration

package runtime_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// volumeCleaner is a VolumeCleaner over the fixture's database and fake API,
// with unusedRetention of one day.
func (f *stopFixture) volumeCleaner() *runtime.VolumeCleaner {
	return &runtime.VolumeCleaner{
		Pool: f.pool, Reader: f.kube, Writer: f.kube, Template: f.catalog.Running, Enabled: true,
		UnusedRetention: 24 * time.Hour, Metrics: f.metrics,
		Now: func() time.Time { return *f.now.Load() },
	}
}

// freeVolume inserts a volume row that no instance holds, with a recorded PV
// of that name, and returns it. The PV is made by the caller.
func (f *stopFixture) freeVolume(t *testing.T, template *v1alpha1.MCPServerTemplate, pvName string) db.PersistentVolume {
	t.Helper()

	instance := f.instance(t, template, "key-"+pvName, "alice", "failed")
	row := f.heldVolume(t, template, instance, "data", pvName)
	_, err := f.queries.ReleasePersistentVolumes(t.Context(), row.InstanceID)
	require.NoError(t, err)

	row, err = f.queries.GetPersistentVolume(t.Context(), row.ID)
	require.NoError(t, err)

	return row
}

func (f *stopFixture) createPV(t *testing.T, row db.PersistentVolume, phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
	t.Helper()

	pv := ownedPV(row, phase, nil)
	pv.Name = row.PvName.String
	pv.UID = types.UID("uid-" + pv.Name)
	require.NoError(t, f.kube.Create(t.Context(), pv))

	return pv
}

func (f *stopFixture) getPV(t *testing.T, name string) (*corev1.PersistentVolume, bool) {
	t.Helper()

	var pv corev1.PersistentVolume

	err := f.kube.Get(t.Context(), types.NamespacedName{Name: name}, &pv)
	if apierrors.IsNotFound(err) {
		return nil, false
	}

	require.NoError(t, err)

	return &pv, true
}

func (f *stopFixture) volumeRow(t *testing.T, id string) (db.PersistentVolume, bool) {
	t.Helper()

	row, err := f.queries.GetPersistentVolume(t.Context(), id)
	if err != nil {
		return db.PersistentVolume{}, false
	}

	return row, true
}

func (f *stopFixture) backdate(t *testing.T, id string, column string, ago time.Duration) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE persistent_volumes SET "+column+" = now() - $2::interval WHERE id = $1", id, ago.String())
	require.NoError(t, err)
}

func (f *stopFixture) volumeEvent(event string) float64 {
	return promtestutil.ToFloat64(f.metrics.PersistentVolumeEvents.WithLabelValues(event))
}

func TestVolumeCleanup(t *testing.T) {
	t.Run("deletes the volume of a deleted template after the reclaim wait", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("gone", "per-user", time.Hour, v1alpha1.Access{})
		row := fixture.freeVolume(t, template, "pv-gone")
		fixture.createPV(t, row, corev1.VolumeReleased)
		fixture.setNow(time.Now())
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		cleaner := fixture.volumeCleaner()
		require.NoError(t, cleaner.Run(t.Context()))

		marked, found := fixture.volumeRow(t, row.ID)
		require.True(t, found)
		assert.Equal(t, "template-deleted", marked.DeleteReason)
		assert.True(t, marked.ReclaimPatchedAt.Valid)
		assert.Equal(t, 1, countAudit(t, fixture.pool, "volume.delete", "template-deleted"))

		pv, found := fixture.getPV(t, "pv-gone")
		require.True(t, found, "a Released PV is left to the PV controller during the reclaim wait")
		assert.Equal(t, corev1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy)

		fixture.setNow(time.Now().Add(runtime.VolumeReclaimWait - time.Minute))
		require.NoError(t, cleaner.Run(t.Context()))

		_, found = fixture.getPV(t, "pv-gone")
		assert.True(t, found, "the wait has not passed")

		fixture.setNow(time.Now().Add(runtime.VolumeReclaimWait + time.Minute))
		require.NoError(t, cleaner.Run(t.Context()))

		_, found = fixture.getPV(t, "pv-gone")
		assert.False(t, found, "the PV is deleted after the wait")

		require.NoError(t, cleaner.Run(t.Context()))

		_, found = fixture.volumeRow(t, row.ID)
		assert.False(t, found, "the row goes once the PV is gone")
		assert.InDelta(t, 1, fixture.volumeEvent(runtime.VolumeEventDeleted), 0)
	})

	t.Run("treats a template with a new UID as deleted and an invalid one as present", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("recreated", "per-user", time.Hour, v1alpha1.Access{})
		recreated := fixture.freeVolume(t, template, "pv-recreated")
		fixture.createPV(t, recreated, corev1.VolumeAvailable)

		invalid := fixture.template("invalid", "per-user", time.Hour, v1alpha1.Access{})
		invalidRow := fixture.freeVolume(t, invalid, "pv-invalid")
		fixture.createPV(t, invalidRow, corev1.VolumeAvailable)
		fixture.catalog.Set(invalid, []string{"broken"})

		fixture.setNow(time.Now())

		fresh := template.DeepCopy()
		fresh.UID = "uid-recreated-new"
		fixture.catalog.Set(fresh, nil)

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		marked, found := fixture.volumeRow(t, recreated.ID)
		require.True(t, found)
		assert.Equal(t, "template-deleted", marked.DeleteReason)

		kept, found := fixture.volumeRow(t, invalidRow.ID)
		require.True(t, found)
		assert.Empty(t, kept.DeleteReason)
	})

	t.Run("deletes an unused volume only when retention is on and the volume is old enough", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("unused", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())

		old := fixture.freeVolume(t, template, "pv-old")
		fixture.createPV(t, old, corev1.VolumeAvailable)
		fixture.backdate(t, old.ID, "last_used_at", 25*time.Hour)

		fresh := fixture.freeVolume(t, template, "pv-fresh")
		fixture.createPV(t, fresh, corev1.VolumeAvailable)
		fixture.backdate(t, fresh.ID, "last_used_at", 23*time.Hour)

		never := fixture.volumeCleaner()
		never.UnusedRetention = 0
		require.NoError(t, never.Run(t.Context()))

		row, _ := fixture.volumeRow(t, old.ID)
		assert.Empty(t, row.DeleteReason, "a retention of 0 never deletes")

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		row, found := fixture.volumeRow(t, old.ID)
		require.True(t, found)
		assert.Equal(t, "unused", row.DeleteReason)

		row, found = fixture.volumeRow(t, fresh.ID)
		require.True(t, found)
		assert.Empty(t, row.DeleteReason)
		assert.Equal(t, 1, countAudit(t, fixture.pool, "volume.delete", "unused"))
	})

	t.Run("deletes a volume an admin marked", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("admin", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		row := fixture.freeVolume(t, template, "pv-admin")
		fixture.createPV(t, row, corev1.VolumeAvailable)

		_, err := fixture.pool.Exec(t.Context(), `UPDATE persistent_volumes SET delete_reason = 'admin', delete_requested_at = now() WHERE id = $1`, row.ID)
		require.NoError(t, err)

		cleaner := fixture.volumeCleaner()
		require.NoError(t, cleaner.Run(t.Context()))

		_, found := fixture.getPV(t, "pv-admin")
		assert.False(t, found, "an Available PV is deleted at once")

		require.NoError(t, cleaner.Run(t.Context()))

		_, found = fixture.volumeRow(t, row.ID)
		assert.False(t, found)
	})

	t.Run("leaves a held volume alone, even when an admin marked it", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("held", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		instance := fixture.instance(t, template, "held-key", "alice", "ready")
		row := fixture.heldVolume(t, template, instance, "data", "pv-held")
		fixture.createPV(t, row, corev1.VolumeBound)
		fixture.backdate(t, row.ID, "last_used_at", 48*time.Hour)
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		_, err := fixture.pool.Exec(t.Context(), `UPDATE persistent_volumes SET delete_reason = 'admin', delete_requested_at = now() WHERE id = $1`, row.ID)
		require.NoError(t, err)

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		after, found := fixture.volumeRow(t, row.ID)
		require.True(t, found)
		assert.Equal(t, instance.ID, after.InstanceID.String)
		assert.False(t, after.ReclaimPatchedAt.Valid)

		pv, found := fixture.getPV(t, "pv-held")
		require.True(t, found)
		assert.Equal(t, corev1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy)
	})

	t.Run("deletes the row of a volume whose PV is missing or never recorded", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("missing", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		missing := fixture.freeVolume(t, template, "pv-missing")
		unrecorded := fixture.freeVolume(t, template, "")

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		_, found := fixture.volumeRow(t, missing.ID)
		assert.False(t, found)

		_, found = fixture.volumeRow(t, unrecorded.ID)
		assert.False(t, found)
		assert.InDelta(t, 0, fixture.volumeEvent(runtime.VolumeEventDeleted), 0, "nothing was deleted by toolgate")
	})

	t.Run("never touches a PV without toolgate's labels and forgets its row", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("labels", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		row := fixture.freeVolume(t, template, "pv-foreign")
		pv := fixture.createPV(t, row, corev1.VolumeAvailable)
		pv.Labels = map[string]string{runtime.LabelVolumeID: "another-row"}
		require.NoError(t, fixture.kube.Update(t.Context(), pv))

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		after, found := fixture.getPV(t, "pv-foreign")
		require.True(t, found)
		assert.Equal(t, corev1.PersistentVolumeReclaimRetain, after.Spec.PersistentVolumeReclaimPolicy)
		assert.Equal(t, map[string]string{runtime.LabelVolumeID: "another-row"}, after.Labels)

		_, found = fixture.volumeRow(t, row.ID)
		assert.False(t, found)
		assert.InDelta(t, 1, fixture.volumeEvent(runtime.VolumeEventDeleteFailed), 0)
	})

	t.Run("sets the Delete policy on a leader's next pass after it died before the patch", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("died", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		row := fixture.freeVolume(t, template, "pv-died")
		fixture.createPV(t, row, corev1.VolumeReleased)

		_, err := fixture.pool.Exec(t.Context(), `UPDATE persistent_volumes SET delete_reason = 'template-deleted',
			delete_requested_at = now(), reclaim_patched_at = now() WHERE id = $1`, row.ID)
		require.NoError(t, err)

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		pv, found := fixture.getPV(t, "pv-died")
		require.True(t, found)
		assert.Equal(t, corev1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy)
	})

	t.Run("waits for a PV still bound elsewhere, then forgets the row after the forget bound", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("bound", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})

		row := fixture.freeVolume(t, template, "pv-bound")
		fixture.createPV(t, row, corev1.VolumeBound)

		cleaner := fixture.volumeCleaner()
		require.NoError(t, cleaner.Run(t.Context()))

		waiting, found := fixture.volumeRow(t, row.ID)
		require.True(t, found)
		assert.NotEmpty(t, waiting.Problem)
		assert.False(t, waiting.ReclaimPatchedAt.Valid, "the PV is not touched while it is bound")

		// One minute short of the bound the row stays; past it the row goes.
		fixture.backdate(t, row.ID, "delete_requested_at", runtime.VolumeForgetWait-time.Minute)
		fixture.backdate(t, row.ID, "last_used_at", runtime.VolumeForgetWait-time.Minute)
		require.NoError(t, cleaner.Run(t.Context()))

		_, found = fixture.volumeRow(t, row.ID)
		assert.True(t, found)

		fixture.backdate(t, row.ID, "delete_requested_at", runtime.VolumeForgetWait+time.Minute)
		fixture.backdate(t, row.ID, "last_used_at", runtime.VolumeForgetWait+time.Minute)
		require.NoError(t, cleaner.Run(t.Context()))

		_, found = fixture.volumeRow(t, row.ID)
		assert.False(t, found)
		assert.InDelta(t, 1, fixture.volumeEvent(runtime.VolumeEventDeleteFailed), 0)

		pv, found := fixture.getPV(t, "pv-bound")
		require.True(t, found)
		assert.Equal(t, corev1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy)
	})

	t.Run("sets the gauges for every series each pass", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("gauges", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		fixture.freeVolume(t, template, "pv-g")

		require.NoError(t, fixture.volumeCleaner().Run(t.Context()))

		assert.InDelta(t, 1, promtestutil.ToFloat64(fixture.metrics.PersistentVolumes.WithLabelValues("per-user", "idle")), 0)
		assert.InDelta(t, 0, promtestutil.ToFloat64(fixture.metrics.PersistentVolumes.WithLabelValues("per-template", "deleting")), 0)
		assert.InDelta(t, float64(gib), promtestutil.ToFloat64(fixture.metrics.PersistentVolumeBytes.WithLabelValues("per-user")), 0)
	})
}

func countAudit(t *testing.T, pool *pgxpool.Pool, kind, reason string) int {
	t.Helper()

	var count int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = $1 AND details->>'reason' = $2", kind, reason).Scan(&count))

	return count
}

func TestVolumeCleanupRacesStart(t *testing.T) {
	fixture := newVolumeStartFixture(t)
	template := fixture.volumeTemplate("data")
	principal := fixture.principal()
	row := fixture.recordedVolume(t, template, principal, "pv-race")

	_, err := fixture.pool.Exec(t.Context(), `UPDATE persistent_volumes SET last_used_at = now() - interval '2 hours' WHERE id = $1`, row.ID)
	require.NoError(t, err)

	pv := ownedPV(row, corev1.VolumeReleased, nil)
	pv.Name = "pv-race"
	pv.UID = "uid-pv-race"

	kube := clientfake.NewClientBuilder().WithObjects(pv).Build()
	cleaner := &runtime.VolumeCleaner{
		Pool: fixture.pool, Reader: kube, Writer: kube, Enabled: true, UnusedRetention: time.Hour,
		Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) { return template.DeepCopy(), true },
	}
	instance := fixture.instance(t, template, principal)

	var (
		group  sync.WaitGroup
		result runtime.VolumeClaimResult
		claimE error
		runE   error
	)

	group.Add(2)

	go func() {
		defer group.Done()

		result, claimE = fixture.store.ClaimVolumes(t.Context(), instance, template.Name, claims(template, "per-user"), 0)
	}()

	go func() {
		defer group.Done()

		runE = cleaner.Run(t.Context())
	}()

	group.Wait()
	require.NoError(t, claimE)
	require.NoError(t, runE)

	after, found := fixture.volumeRow(t, row.ID)
	require.True(t, found, "the row is never deleted in this pass")

	var stored corev1.PersistentVolume
	require.NoError(t, kube.Get(t.Context(), types.NamespacedName{Name: "pv-race"}, &stored))

	if len(result.Rows) > 0 {
		assert.Equal(t, instance.ID, after.InstanceID.String)
		assert.Empty(t, after.DeleteReason, "the claim cleared the mark")
		assert.Equal(t, corev1.PersistentVolumeReclaimRetain, stored.Spec.PersistentVolumeReclaimPolicy, "the data stays")
		assert.False(t, after.ReclaimPatchedAt.Valid)

		return
	}

	assert.Equal(t, "Volume data is being deleted.", result.Wait)
	assert.Equal(t, "unused", after.DeleteReason)
	assert.True(t, after.ReclaimPatchedAt.Valid)
	assert.False(t, after.InstanceID.Valid)
}

// volumeFault is a client whose writes fail from the (limit+1)-th on, as when
// the leader dies in the middle of a step. writes counts the attempts.
type volumeFault struct {
	client.Client
	writes atomic.Int64
}

var errLeaderDied = errors.New("leader died")

func newVolumeFault(base client.WithWatch, limit int64) *volumeFault {
	fault := &volumeFault{}
	count := func() error {
		if fault.writes.Add(1) > limit {
			return errLeaderDied
		}

		return nil
	}

	fault.Client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, kube client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := count(); err != nil {
				return err
			}

			return kube.Create(ctx, object, opts...)
		},
		Update: func(ctx context.Context, kube client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if err := count(); err != nil {
				return err
			}

			return kube.Update(ctx, object, opts...)
		},
		Patch: func(ctx context.Context, kube client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := count(); err != nil {
				return err
			}

			return kube.Patch(ctx, object, patch, opts...)
		},
		Delete: func(ctx context.Context, kube client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			if err := count(); err != nil {
				return err
			}

			return kube.Delete(ctx, object, opts...)
		},
		Apply: func(ctx context.Context, kube client.WithWatch, object k8sruntime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if err := count(); err != nil {
				return err
			}

			return reapplyFakeApply(ctx, kube, object, opts...)
		},
	})

	return fault
}

const maxFaultPoints = 40

func TestVolumeStepsRepeatAfterLeaderChange(t *testing.T) {
	t.Run("a start stopped after any step is finished by a fresh starter", func(t *testing.T) {
		fixture := newVolumeStartFixture(t)

		for limit := int64(0); limit < maxFaultPoints; limit++ {
			template := fixture.volumeTemplate("data")
			principal := fixture.principal()
			row := fixture.recordedVolume(t, template, principal, "pv-start")
			pv := ownedPV(row, corev1.VolumeReleased, nil)
			pv.Name = "pv-start"

			started := fixture.starter(t, template, 0, pv, storageClass("retain", &retainPolicy))
			fault := newVolumeFault(started.kube.(client.WithWatch), limit)
			started.starter.Client = fault

			instance := fixture.instance(t, template, principal)
			_ = started.starter.Step(t.Context(), fixture.get(t, instance.ID))
			interrupted := fault.writes.Load() > limit

			started.starter.Client = started.kube
			final := step(t, started, fixture, instance.ID)
			assert.Equal(t, runtime.StatusStarting, final.Status, "limit %d: %s", limit, final.Reason)

			var bound corev1.PersistentVolume
			require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Name: "pv-start"}, &bound))
			require.NotNil(t, bound.Spec.ClaimRef, "limit %d", limit)
			assert.Equal(t, "tg-"+instance.ID, bound.Spec.ClaimRef.Namespace)

			var claim corev1.PersistentVolumeClaim
			require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Namespace: "tg-" + instance.ID, Name: "data"}, &claim))
			assert.Equal(t, "pv-start", claim.Spec.VolumeName)

			if !interrupted {
				return
			}
		}

		t.Fatal("the start never finished within the fault points")
	})

	t.Run("a teardown stopped after any step is finished by a fresh teardown", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("repeat", "per-user", time.Hour, v1alpha1.Access{})
		replicas := int32(1)
		base := fixture.kube
		fixture.setNow(time.Now())

		for limit := int64(0); limit < maxFaultPoints; limit++ {
			instance := fixture.instance(t, template, "repeat-key-"+time.Now().String(), "alice", "failed")
			row := fixture.heldVolume(t, template, instance, "data", "pv-teardown")
			require.NoError(t, base.Create(t.Context(), &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instance.Namespace},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			}))

			fault := newVolumeFault(base.(client.WithWatch), limit)
			fixture.kube = fault

			_, _ = fixture.volumeTeardown().Advance(t.Context(), instance)
			interrupted := fault.writes.Load() > limit

			fixture.kube = base

			for range 3 {
				_, err := fixture.volumeTeardown().Advance(t.Context(), fixture.get(t, instance.ID))
				require.NoError(t, err)
			}

			assert.Equal(t, "stopped", fixture.get(t, instance.ID).Status, "limit %d", limit)
			assert.False(t, fixture.namespaceExists(t, instance.Namespace), "limit %d", limit)
			assert.False(t, fixture.holder(t, row.ID).Valid, "limit %d", limit)

			if !interrupted {
				return
			}
		}

		t.Fatal("the teardown never finished within the fault points")
	})

	t.Run("a clean-up stopped after any step is finished by a fresh cleaner", func(t *testing.T) {
		fixture := newStopFixture(t)
		template := fixture.template("cleanrepeat", "per-user", time.Hour, v1alpha1.Access{})
		fixture.setNow(time.Now())
		fixture.catalog.Delete(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})
		base := fixture.kube

		for limit := int64(0); limit < maxFaultPoints; limit++ {
			row := fixture.freeVolume(t, template, "pv-clean")
			pv := ownedPV(row, corev1.VolumeAvailable, nil)
			pv.Name = "pv-clean"
			pv.UID = "uid-pv-clean"
			require.NoError(t, base.Create(t.Context(), pv))

			fault := newVolumeFault(base.(client.WithWatch), limit)
			fixture.kube = fault
			_ = fixture.volumeCleaner().Run(t.Context())
			interrupted := fault.writes.Load() > limit

			fixture.kube = base

			for range 3 {
				require.NoError(t, fixture.volumeCleaner().Run(t.Context()))
			}

			_, found := fixture.getPV(t, "pv-clean")
			assert.False(t, found, "limit %d", limit)

			_, found = fixture.volumeRow(t, row.ID)
			assert.False(t, found, "limit %d", limit)

			if !interrupted {
				return
			}
		}

		t.Fatal("the clean-up never finished within the fault points")
	})
}

func TestVolumeFeatureOff(t *testing.T) {
	fixture := newVolumeStartFixture(t)
	template := fixture.volumeTemplate("data")
	validator := catalog.Validator{PersistentVolumes: false}
	assert.NotEmpty(t, validator.Validate(template), "a template with persistentVolumes is invalid while the setting is off")

	principal := fixture.principal()
	row := fixture.recordedVolume(t, template, principal, "pv-off")
	pv := ownedPV(row, corev1.VolumeAvailable, nil)
	pv.Name = "pv-off"
	pv.UID = "uid-pv-off"

	started := fixture.starter(t, template, 0, pv, storageClass("retain", &retainPolicy))
	started.starter.PersistentVolumesEnabled = false

	instance := fixture.instance(t, template, principal)
	failed := step(t, started, fixture, instance.ID)
	assert.Equal(t, "failed", failed.Status)
	assert.Equal(t, runtime.FeatureOffReason, failed.Reason)

	t.Run("a stop still releases a held volume", func(t *testing.T) {
		held := fixture.instance(t, template, fixture.principal())
		result, err := fixture.store.ClaimVolumes(t.Context(), held, template.Name, claims(template, "per-user"), 0)
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)

		fixture.stop(t, held.ID)

		released, err := fixture.queries.GetPersistentVolume(t.Context(), result.Rows[0].ID)
		require.NoError(t, err)
		assert.False(t, released.InstanceID.Valid)
	})

	t.Run("no clean-up deletes anything", func(t *testing.T) {
		_, err := fixture.pool.Exec(t.Context(), `UPDATE persistent_volumes SET delete_reason = 'admin', delete_requested_at = now() WHERE id = $1`, row.ID)
		require.NoError(t, err)

		cleaner := &runtime.VolumeCleaner{
			Pool: fixture.pool, Reader: started.kube, Writer: started.kube, Enabled: false, UnusedRetention: time.Hour,
			Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) { return nil, false },
		}
		require.NoError(t, cleaner.Run(t.Context()))

		kept, found := fixture.volumeRow(t, row.ID)
		require.True(t, found)
		assert.False(t, kept.ReclaimPatchedAt.Valid)

		var stored corev1.PersistentVolume
		require.NoError(t, started.kube.Get(t.Context(), types.NamespacedName{Name: "pv-off"}, &stored))
		assert.Equal(t, corev1.PersistentVolumeReclaimRetain, stored.Spec.PersistentVolumeReclaimPolicy)
	})
}

func (f *volumeStartFixture) volumeRow(t *testing.T, id string) (db.PersistentVolume, bool) {
	t.Helper()

	row, err := f.queries.GetPersistentVolume(t.Context(), id)
	if err != nil {
		return db.PersistentVolume{}, false
	}

	return row, true
}
