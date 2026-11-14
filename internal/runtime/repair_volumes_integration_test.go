//go:build integration

package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

// volumeRepairSetup returns an unhealthy ready instance that holds a
// recorded volume "cache" on PV "pv-cache", with a ConfigMap template
// object, and a cluster that records the kinds applied, in order.
func volumeRepairSetup(t *testing.T, fixture *stopFixture, name string, pvLabels map[string]string) (db.Instance, db.PersistentVolume, client.Client, *[]string) {
	t.Helper()

	everyone := v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"}
	template := fixture.template(name, "per-user", time.Hour, everyone)
	template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"user":"x"}}`)}}
	fixture.catalog.Set(template, nil)

	session := fixture.session(t, name+"-user", "s1", "", "staff")
	fixture.enable(t, session, template)
	instance := fixture.instance(t, template, "user:"+name+"-user", name+"-user", "ready")
	_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET render_session = $2 WHERE id = $1", instance.ID, session.ID)
	require.NoError(t, err)

	rowID := "feedfeedfeedfeedfeedfeedfeedfeed"
	_, err = db.New(fixture.pool).InsertPersistentVolume(t.Context(), db.InsertPersistentVolumeParams{
		ID: rowID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		VolumeName: "cache", Scope: "per-user", ScopeID: "user:" + name + "-user", StorageClass: "retain", SizeBytes: 1 << 30,
	})
	require.NoError(t, err)
	_, err = fixture.pool.Exec(t.Context(),
		"UPDATE persistent_volumes SET pv_name = 'pv-cache', instance_id = $2 WHERE id = $1", rowID, instance.ID)
	require.NoError(t, err)

	row, err := db.New(fixture.pool).GetPersistentVolume(t.Context(), rowID)
	require.NoError(t, err)

	instance = fixture.makeUnhealthy(t, fixture.get(t, instance.ID), healthCheck("reapply", 3), (*fixture.now.Load()).Add(-time.Second))

	if pvLabels == nil {
		pvLabels = map[string]string{
			runtime.LabelVolumeID: row.ID, runtime.LabelTemplateUID: row.TemplateUid, runtime.LabelManagedBy: "toolgate",
		}
	}

	volume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-cache", Labels: pvLabels},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	applied := &[]string{}
	kube := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(volume).WithStatusSubresource(volume).
		WithInterceptorFuncs(interceptor.Funcs{Apply: func(ctx context.Context, kube client.WithWatch,
			object k8sruntime.ApplyConfiguration, options ...client.ApplyOption,
		) error {
			if kind, ok := object.(interface{ GetKind() string }); ok {
				*applied = append(*applied, kind.GetKind())
			}

			return reapplyFakeApply(ctx, kube, object, options...)
		}}).Build()

	return instance, row, kube, applied
}

func TestReapplyRecreatesVolumeClaims(t *testing.T) {
	fixture := newStopFixture(t)
	defer fixture.setNow(time.Now())

	instance, row, kube, applied := volumeRepairSetup(t, fixture, "reapply-volume", nil)

	require.NoError(t, fixture.repairer(kube).Step(t.Context(), instance))

	claim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: "cache"}, claim),
		"the deleted claim is applied again")
	assert.Equal(t, "pv-cache", claim.Spec.VolumeName)
	assert.Equal(t, row.VolumeName, claim.Labels[runtime.LabelVolume])

	volume := &corev1.PersistentVolume{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Name: "pv-cache"}, volume))
	require.NotNil(t, volume.Spec.ClaimRef, "the free PV is pre-bound to the claim")
	assert.Equal(t, "cache", volume.Spec.ClaimRef.Name)

	require.Contains(t, *applied, "PersistentVolumeClaim")
	require.Contains(t, *applied, "ConfigMap")
	assert.Less(t, indexOf(*applied, "PersistentVolumeClaim"), indexOf(*applied, "ConfigMap"),
		"the claim is applied before the template objects")
}

func TestReapplyFailsWhenRecordedVolumeHasWrongLabels(t *testing.T) {
	fixture := newStopFixture(t)
	defer fixture.setNow(time.Now())

	instance, row, kube, _ := volumeRepairSetup(t, fixture, "reapply-volume-labels",
		map[string]string{runtime.LabelManagedBy: "someone"})

	err := fixture.repairer(kube).Step(t.Context(), instance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "doesn't carry toolgate's labels for volume cache")

	claim := &corev1.PersistentVolumeClaim{}
	assert.Error(t, kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: "cache"}, claim))

	stored, getErr := db.New(fixture.pool).GetPersistentVolume(t.Context(), row.ID)
	require.NoError(t, getErr)
	assert.NotEmpty(t, stored.Problem)
}

func indexOf(items []string, want string) int {
	for index, item := range items {
		if item == want {
			return index
		}
	}

	return -1
}
