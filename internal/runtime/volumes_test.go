package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const volumeRowID = "0123456789abcdef0123456789abcdef"

func volumeFixture() (db.PersistentVolume, db.Instance, *v1alpha1.MCPServerTemplate) {
	row := db.PersistentVolume{
		ID: volumeRowID, TemplateUid: "tpl-uid", VolumeName: "cache", StorageClass: "retain",
		SizeBytes: 1 << 30,
	}
	instance := db.Instance{ID: "inst1", Scope: "per-user", Generation: 3, TemplateUid: "tpl-uid", OwnerPrincipal: "alice"}
	template := &v1alpha1.MCPServerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "toolgate"}}

	return row, instance, template
}

func TestRenderVolumeClaim(t *testing.T) {
	row, instance, template := volumeFixture()
	meta := runtime.NewInstanceMetadata(instance, template, runtime.Identity{}, "v1", false)

	claim := runtime.RenderVolumeClaim(row, instance, template, meta)

	assert.Equal(t, "cache", claim.Name)
	assert.Equal(t, "tg-inst1", claim.Namespace)
	assert.Equal(t, "toolgate", claim.Labels[runtime.LabelManagedBy])
	assert.Equal(t, "inst1", claim.Labels[runtime.LabelInstance])
	assert.Equal(t, "web", claim.Labels[runtime.LabelTemplate])
	assert.Equal(t, "cache", claim.Labels[runtime.LabelVolume])
	assert.Equal(t, "3", claim.Annotations[runtime.AnnotationTemplateGeneration])
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes)
	assert.Equal(t, corev1.PersistentVolumeFilesystem, *claim.Spec.VolumeMode)
	assert.Equal(t, "retain", *claim.Spec.StorageClassName)
	assert.Equal(t, int64(1<<30), claim.Spec.Resources.Requests.Storage().Value())
	assert.Empty(t, claim.Spec.VolumeName)

	row.PvName = pgtype.Text{String: "pvc-abc", Valid: true}
	row.SizeBytes = 2 << 30
	claim = runtime.RenderVolumeClaim(row, instance, template, meta)
	assert.Equal(t, "pvc-abc", claim.Spec.VolumeName)
	assert.Equal(t, int64(2<<30), claim.Spec.Resources.Requests.Storage().Value())
}

func storageClass(name string, policy *corev1.PersistentVolumeReclaimPolicy) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "x", ReclaimPolicy: policy}
}

func ownedPV(row db.PersistentVolume, phase corev1.PersistentVolumePhase, ref *corev1.ObjectReference) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1", Labels: map[string]string{
			runtime.LabelVolumeID: row.ID, runtime.LabelTemplateUID: row.TemplateUid, runtime.LabelManagedBy: "toolgate",
		}},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: ref,
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase, Message: "boom"},
	}
}

func TestVolumeStartChecks(t *testing.T) {
	retain, deletePolicy := corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete
	row, instance, _ := volumeFixture()
	row.PvName = pgtype.Text{String: "pv-1", Valid: true}
	ours := &corev1.ObjectReference{Namespace: "tg-inst1", Name: "cache", UID: "old-uid"}
	other := &corev1.ObjectReference{Namespace: "tg-old", Name: "cache"}

	t.Run("storage class", func(t *testing.T) {
		for name, test := range map[string]struct {
			class  *storagev1.StorageClass
			outome runtime.VolumeStep
		}{
			"missing": {nil, runtime.VolumeStep{Outcome: runtime.VolumeFail, Reason: "StorageClass retain for volume cache doesn't exist."}},
			"delete":  {storageClass("retain", &deletePolicy), runtime.VolumeStep{Outcome: runtime.VolumeFail, Reason: "StorageClass retain for volume cache has reclaimPolicy Delete. Persistent volumes need Retain, or the data is deleted when the instance stops."}},
			"unset":   {storageClass("retain", nil), runtime.VolumeStep{Outcome: runtime.VolumeFail, Reason: "StorageClass retain for volume cache has reclaimPolicy Delete. Persistent volumes need Retain, or the data is deleted when the instance stops."}},
			"retain":  {storageClass("retain", &retain), runtime.VolumeStep{Outcome: runtime.VolumeGoOn}},
		} {
			t.Run(name, func(t *testing.T) {
				builder := fake.NewClientBuilder()
				if test.class != nil {
					builder = builder.WithObjects(test.class)
				}

				step, err := runtime.CheckStorageClass(t.Context(), builder.Build(), row)
				require.NoError(t, err)
				assert.Equal(t, test.outome, step)
			})
		}
	})

	t.Run("recorded volume", func(t *testing.T) {
		wrongID := ownedPV(row, corev1.VolumeAvailable, nil)
		wrongID.Labels[runtime.LabelVolumeID] = "other"
		wrongUID := ownedPV(row, corev1.VolumeAvailable, nil)
		wrongUID.Labels[runtime.LabelTemplateUID] = "other"
		wrongManager := ownedPV(row, corev1.VolumeAvailable, nil)
		delete(wrongManager.Labels, runtime.LabelManagedBy)

		wrongReclaim := ownedPV(row, corev1.VolumeAvailable, nil)
		wrongReclaim.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete

		labelReason := "PersistentVolume pv-1 doesn't carry toolgate's labels for volume cache, so toolgate won't use it."

		for name, test := range map[string]struct {
			pv   *corev1.PersistentVolume
			want runtime.VolumeStep
		}{
			"not found":           {nil, runtime.VolumeStep{Outcome: runtime.VolumeLost}},
			"wrong id":            {wrongID, runtime.VolumeStep{Outcome: runtime.VolumeFail, Problem: true, Reason: labelReason}},
			"wrong uid":           {wrongUID, runtime.VolumeStep{Outcome: runtime.VolumeFail, Problem: true, Reason: labelReason}},
			"wrong manager":       {wrongManager, runtime.VolumeStep{Outcome: runtime.VolumeFail, Problem: true, Reason: labelReason}},
			"reclaim":             {wrongReclaim, runtime.VolumeStep{Outcome: runtime.VolumeFail, Problem: true, Reason: "PersistentVolume pv-1 has reclaim policy Delete, so toolgate won't bind it."}},
			"bound ours":          {ownedPV(row, corev1.VolumeBound, ours), runtime.VolumeStep{Outcome: runtime.VolumeGoOn}},
			"bound other":         {ownedPV(row, corev1.VolumeBound, other), runtime.VolumeStep{Outcome: runtime.VolumeWait, Reason: "Volume cache is still bound to tg-old/cache."}},
			"released":            {ownedPV(row, corev1.VolumeReleased, other), runtime.VolumeStep{Outcome: runtime.VolumeGoOn, NeedsPreBind: true}},
			"available":           {ownedPV(row, corev1.VolumeAvailable, nil), runtime.VolumeStep{Outcome: runtime.VolumeGoOn, NeedsPreBind: true}},
			"available pre-bound": {ownedPV(row, corev1.VolumeAvailable, ours), runtime.VolumeStep{Outcome: runtime.VolumeGoOn}},
			"pending":             {ownedPV(row, corev1.VolumePending, nil), runtime.VolumeStep{Outcome: runtime.VolumeFail, Reason: "PersistentVolume pv-1 is Pending: boom"}},
			"failed":              {ownedPV(row, corev1.VolumeFailed, nil), runtime.VolumeStep{Outcome: runtime.VolumeFail, Reason: "PersistentVolume pv-1 is Failed: boom"}},
		} {
			t.Run(name, func(t *testing.T) {
				builder := fake.NewClientBuilder()
				if test.pv != nil {
					builder = builder.WithObjects(test.pv)
				}

				step, err := runtime.CheckRecordedVolume(t.Context(), builder.Build(), row, instance)
				require.NoError(t, err)

				step.PV = nil
				assert.Equal(t, test.want, step)
			})
		}
	})

	t.Run("pre-bind body", func(t *testing.T) {
		old := &corev1.ObjectReference{Namespace: "tg-old", Name: "cache", UID: "old-uid", ResourceVersion: "42"}
		pv := ownedPV(row, corev1.VolumeReleased, old)

		var body []byte

		writer := fake.NewClientBuilder().WithObjects(pv).WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				var err error

				body, err = patch.Data(obj)
				require.NoError(t, err)

				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

		var current corev1.PersistentVolume
		require.NoError(t, writer.Get(t.Context(), types.NamespacedName{Name: "pv-1"}, &current))
		require.NoError(t, runtime.PreBind(t.Context(), writer, &current, instance, "cache"))

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))

		spec := decoded["spec"].(map[string]any)["claimRef"].(map[string]any)
		assert.Contains(t, spec, "uid")
		assert.Nil(t, spec["uid"])
		assert.Contains(t, spec, "resourceVersion")
		assert.Nil(t, spec["resourceVersion"])
		assert.Equal(t, "tg-inst1", spec["namespace"])

		metadata := decoded["metadata"].(map[string]any)
		assert.NotEmpty(t, metadata["resourceVersion"])

		require.NoError(t, writer.Get(t.Context(), types.NamespacedName{Name: "pv-1"}, &current))
		assert.Equal(t, "tg-inst1", current.Spec.ClaimRef.Namespace)
		assert.Empty(t, current.Spec.ClaimRef.UID)
	})
}

type recordCase struct {
	pv    *corev1.PersistentVolume
	claim *corev1.PersistentVolumeClaim
}

func recordFixture(row db.PersistentVolume) recordCase {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "tg-inst1", UID: "claim-uid"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-claim-uid"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-claim-uid", Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "csi"}},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{UID: "claim-uid", Namespace: "tg-inst1", Name: "cache"},
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
		},
	}

	return recordCase{pv: pv, claim: claim}
}

// recordAndCheckUnchanged runs recordVolume and asserts a refusal changed
// nothing on the PV.
func recordRefused(t *testing.T, fixture recordCase, reason string) {
	t.Helper()

	row, instance, template := volumeFixture()
	writer := fake.NewClientBuilder().WithObjects(fixture.pv, fixture.claim).Build()

	var before corev1.PersistentVolume
	require.NoError(t, writer.Get(t.Context(), types.NamespacedName{Name: fixture.pv.Name}, &before))

	step, err := runtime.RecordVolume(t.Context(), writer, writer, row, instance, template)
	require.NoError(t, err)
	assert.Equal(t, runtime.VolumeFail, step.Outcome)
	assert.Equal(t, reason, step.Reason)
	assert.True(t, step.Problem)

	var after corev1.PersistentVolume
	require.NoError(t, writer.Get(t.Context(), types.NamespacedName{Name: fixture.pv.Name}, &after))
	assert.Equal(t, before, after)
}

func TestRecordRefusesAnotherVolumesPV(t *testing.T) {
	row, _, _ := volumeFixture()
	fixture := recordFixture(row)
	fixture.pv.Labels = map[string]string{runtime.LabelVolumeID: "another-row"}

	recordRefused(t, fixture, "PersistentVolume pvc-claim-uid belongs to another volume.")
}

func TestRecordRefusesPVNotProvisionedForTheClaim(t *testing.T) {
	row, _, _ := volumeFixture()

	t.Run("another name", func(t *testing.T) {
		fixture := recordFixture(row)
		fixture.pv.Name = "preexisting"
		fixture.claim.Spec.VolumeName = "preexisting"
		recordRefused(t, fixture, "PersistentVolume preexisting wasn't provisioned for volume cache.")
	})

	t.Run("no provisioned-by annotation", func(t *testing.T) {
		fixture := recordFixture(row)
		fixture.pv.Annotations = nil
		recordRefused(t, fixture, "PersistentVolume pvc-claim-uid wasn't provisioned for volume cache.")
	})

	t.Run("another claim uid", func(t *testing.T) {
		fixture := recordFixture(row)
		fixture.pv.Spec.ClaimRef.UID = "someone-else"
		recordRefused(t, fixture, "PersistentVolume pvc-claim-uid wasn't provisioned for volume cache.")
	})
}

func TestRecordVolumeLabelsProvisionedPV(t *testing.T) {
	row, instance, template := volumeFixture()
	fixture := recordFixture(row)
	writer := fake.NewClientBuilder().WithObjects(fixture.pv, fixture.claim).Build()

	step, err := runtime.RecordVolume(t.Context(), writer, writer, row, instance, template)
	require.NoError(t, err)
	assert.Equal(t, runtime.VolumeGoOn, step.Outcome)
	assert.Equal(t, "pvc-claim-uid", step.PVName)
	assert.Equal(t, int64(2<<30), step.SizeBytes)

	var pv corev1.PersistentVolume
	require.NoError(t, writer.Get(t.Context(), types.NamespacedName{Name: "pvc-claim-uid"}, &pv))
	assert.Equal(t, map[string]string{
		runtime.LabelManagedBy: "toolgate", runtime.LabelTemplate: "web", runtime.LabelTemplateUID: "tpl-uid",
		runtime.LabelVolume: "cache", runtime.LabelVolumeID: volumeRowID,
	}, pv.Labels)
}

func TestRecordVolumeWaitsForUnboundClaim(t *testing.T) {
	row, instance, template := volumeFixture()
	fixture := recordFixture(row)
	fixture.claim.Status.Phase = corev1.ClaimPending
	writer := fake.NewClientBuilder().WithObjects(fixture.pv, fixture.claim).Build()

	step, err := runtime.RecordVolume(t.Context(), writer, writer, row, instance, template)
	require.NoError(t, err)
	assert.Equal(t, runtime.VolumeWait, step.Outcome)
}
