//go:build integration

package runtime_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// volumeTeardown returns a teardown with the volume feature on, reading and
// writing the fixture's fake API and using its clock.
func (f *stopFixture) volumeTeardown() *runtime.Teardown {
	fence := &fakeFence{}
	fence.holds.Store(true)

	return &runtime.Teardown{
		Pool: f.pool, Reader: f.kube, Deleter: &runtime.Reconciler{Fence: fence, Client: f.kube},
		Log: slog.New(slog.NewTextHandler(f.logs, nil)), Metrics: f.metrics,
		Now:    func() time.Time { return *f.now.Load() },
		Writer: f.kube, VolumesEnabled: true,
	}
}

// heldVolume inserts a volume row held by the instance. pvName is empty for
// a volume that isn't recorded yet.
func (f *stopFixture) heldVolume(t *testing.T, template *v1alpha1.MCPServerTemplate, instance db.Instance, name, pvName string) db.PersistentVolume {
	t.Helper()

	id := fmt.Sprintf("%032x", f.counter.Add(1)+1<<40)
	_, err := f.queries.InsertPersistentVolume(t.Context(), db.InsertPersistentVolumeParams{
		ID: id, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		VolumeName: name, Scope: "per-user", ScopeID: instance.ID, StorageClass: "retain", SizeBytes: 1 << 30,
	})
	require.NoError(t, err)

	_, err = f.queries.SetPersistentVolumeHolder(t.Context(), db.SetPersistentVolumeHolderParams{
		ID: id, InstanceID: pgtype.Text{String: instance.ID, Valid: true},
	})
	require.NoError(t, err)

	if pvName != "" {
		_, err = f.pool.Exec(t.Context(), `UPDATE persistent_volumes SET pv_name = $2 WHERE id = $1`, id, pvName)
		require.NoError(t, err)
	}

	row, err := f.queries.GetPersistentVolume(t.Context(), id)
	require.NoError(t, err)

	return row
}

func (f *stopFixture) volumePod(t *testing.T, instance db.Instance, claim string) {
	t.Helper()

	require.NoError(t, f.kube.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instance.Namespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}))
}

func (f *stopFixture) holder(t *testing.T, volumeID string) pgtype.Text {
	t.Helper()

	row, err := f.queries.GetPersistentVolume(t.Context(), volumeID)
	require.NoError(t, err)

	return row.InstanceID
}

func TestTeardownDrainsVolumePods(t *testing.T) {
	fixture := newStopFixture(t)
	template := fixture.template("drain", "per-user", time.Hour, v1alpha1.Access{})
	replicas := int32(2)

	// stoppingInstance returns an instance in its own namespace that holds a
	// recorded volume, runs a pod that mounts it and a Deployment, and is
	// stopping.
	stoppingInstance := func(t *testing.T) (db.Instance, db.PersistentVolume) {
		t.Helper()

		instance := fixture.instance(t, template, "alice", "alice", "failed")
		row := fixture.heldVolume(t, template, instance, "data", "pv-1")
		fixture.volumePod(t, instance, "data")
		require.NoError(t, fixture.kube.Create(t.Context(), &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instance.Namespace},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		}))
		fixture.setNow(time.Now())

		_, err := fixture.volumeTeardown().Advance(t.Context(), instance)
		require.NoError(t, err)

		return fixture.get(t, instance.ID), row
	}

	t.Run("waits for a pod that mounts a held claim, then deletes the namespace and releases the volume", func(t *testing.T) {
		instance, row := stoppingInstance(t)

		assert.True(t, fixture.namespaceExists(t, instance.Namespace))
		assert.Equal(t, "stopping", instance.Status)
		assert.Equal(t, "Waiting for pods to stop before the namespace is deleted.", instance.Reason)

		var deployment appsv1.Deployment
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Namespace: instance.Namespace, Name: "server"}, &deployment))
		assert.Equal(t, int32(0), *deployment.Spec.Replicas)

		// The pod is still there on the next pass.
		_, err := fixture.volumeTeardown().Advance(t.Context(), instance)
		require.NoError(t, err)
		assert.True(t, fixture.namespaceExists(t, instance.Namespace))
		assert.True(t, fixture.holder(t, row.ID).Valid)

		require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instance.Namespace}}))

		_, err = fixture.volumeTeardown().Advance(t.Context(), instance)
		require.NoError(t, err)
		assert.False(t, fixture.namespaceExists(t, instance.Namespace))
		assert.True(t, fixture.holder(t, row.ID).Valid, "the volume is held until the row is stopped")

		_, err = fixture.volumeTeardown().Advance(t.Context(), fixture.get(t, instance.ID))
		require.NoError(t, err)
		assert.Equal(t, "stopped", fixture.get(t, instance.ID).Status)
		assert.False(t, fixture.holder(t, row.ID).Valid)
	})

	t.Run("deletes the namespace once the bound from stopping_at has passed, even with pods", func(t *testing.T) {
		before := promtestutil.ToFloat64(fixture.metrics.PersistentVolumeEvents.WithLabelValues(runtime.VolumeEventDrainTimeout))
		instance, _ := stoppingInstance(t)
		stoppedAt := instance.StoppingAt.Time

		// One nanosecond short of the bound the pod still holds the namespace.
		fixture.setNow(stoppedAt.Add(runtime.VolumeDrainTimeout - time.Nanosecond))
		_, err := fixture.volumeTeardown().Advance(t.Context(), instance)
		require.NoError(t, err)
		assert.True(t, fixture.namespaceExists(t, instance.Namespace))

		fixture.setNow(stoppedAt.Add(runtime.VolumeDrainTimeout))
		_, err = fixture.volumeTeardown().Advance(t.Context(), instance)
		require.NoError(t, err)
		assert.False(t, fixture.namespaceExists(t, instance.Namespace))
		assert.InDelta(t, before+1, promtestutil.ToFloat64(fixture.metrics.PersistentVolumeEvents.WithLabelValues(runtime.VolumeEventDrainTimeout)), 0)
	})

	t.Run("treats a missing stopping_at as a passed bound", func(t *testing.T) {
		instance, _ := stoppingInstance(t)

		_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET stopping_at = NULL WHERE id = $1`, instance.ID)
		require.NoError(t, err)

		_, err = fixture.volumeTeardown().Advance(t.Context(), fixture.get(t, instance.ID))
		require.NoError(t, err)
		assert.False(t, fixture.namespaceExists(t, instance.Namespace))
	})

	t.Run("deletes the namespace at once when no pod mounts a held claim", func(t *testing.T) {
		instance := fixture.instance(t, template, "alice", "alice", "failed")
		fixture.heldVolume(t, template, instance, "data", "pv-2")
		fixture.volumePod(t, instance, "other")
		fixture.setNow(time.Now())

		_, err := fixture.volumeTeardown().Advance(t.Context(), instance)
		require.NoError(t, err)
		assert.False(t, fixture.namespaceExists(t, instance.Namespace))
	})

	t.Run("does no volume work when the feature is off", func(t *testing.T) {
		instance := fixture.instance(t, template, "alice", "alice", "failed")
		fixture.heldVolume(t, template, instance, "data", "pv-3")
		fixture.volumePod(t, instance, "data")
		fixture.setNow(time.Now())

		teardown := fixture.volumeTeardown()
		teardown.VolumesEnabled = false

		_, err := teardown.Advance(t.Context(), instance)
		require.NoError(t, err)
		assert.False(t, fixture.namespaceExists(t, instance.Namespace))
	})

	t.Run("records an unrecorded bound PV, drains and releases the volume", func(t *testing.T) {
		instance := fixture.instance(t, template, "alice", "alice", "failed")
		row := fixture.heldVolume(t, template, instance, "data", "")

		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: instance.Namespace, UID: "claim-uid"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-claim-uid"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		require.NoError(t, fixture.kube.Create(t.Context(), claim))
		require.NoError(t, fixture.kube.Create(t.Context(), &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-claim-uid", Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "csi"}},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				ClaimRef:                      &corev1.ObjectReference{Namespace: instance.Namespace, Name: "data", UID: "claim-uid"},
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			},
		}))
		fixture.volumePod(t, instance, "data")
		fixture.setNow(time.Now())

		teardown := fixture.volumeTeardown()
		_, err := teardown.Advance(t.Context(), instance)
		require.NoError(t, err)

		recorded, err := fixture.queries.GetPersistentVolume(t.Context(), row.ID)
		require.NoError(t, err)
		assert.Equal(t, "pvc-claim-uid", recorded.PvName.String)
		assert.Equal(t, int64(2<<30), recorded.SizeBytes)

		var pv corev1.PersistentVolume
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Name: "pvc-claim-uid"}, &pv))
		assert.Equal(t, row.ID, pv.Labels[runtime.LabelVolumeID])
		assert.True(t, fixture.namespaceExists(t, instance.Namespace), "the pod still runs")

		require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instance.Namespace}}))

		_, err = teardown.Advance(t.Context(), fixture.get(t, instance.ID))
		require.NoError(t, err)
		_, err = teardown.Advance(t.Context(), fixture.get(t, instance.ID))
		require.NoError(t, err)

		assert.Equal(t, "stopped", fixture.get(t, instance.ID).Status)
		assert.False(t, fixture.holder(t, row.ID).Valid)
	})
}

func TestSweepSkipsStoppingNamespaces(t *testing.T) {
	fixture := newStopFixture(t)
	template := fixture.template("sweepstop", "per-user", time.Hour, v1alpha1.Access{})

	stopping := fixture.instance(t, template, "alice", "alice", "ready")
	_, err := fixture.queries.SetInstanceDesiredStopped(t.Context(), stopping.ID)
	require.NoError(t, err)

	changed, err := fixture.queries.SetInstanceStopping(t.Context(), db.SetInstanceStoppingParams{ID: stopping.ID, FromStatus: "ready"})
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)

	fixture.createNamespace(t, "tg-orphan", "orphan")

	fixture.sweep(t, fixture.kube)

	assert.True(t, fixture.namespaceExists(t, stopping.Namespace), "teardown deletes the namespace of a stopping instance")
	assert.False(t, fixture.namespaceExists(t, "tg-orphan"), "a namespace of no instance is still deleted")
}
