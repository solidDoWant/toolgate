package runtime

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Labels and annotations of persistent volumes.
const (
	LabelVolume         = "toolgate.soliddowant.dev/volume"
	LabelVolumeID       = "toolgate.soliddowant.dev/volume-id"
	LabelTemplateUID    = "toolgate.soliddowant.dev/template-uid"
	annotationPVProvBy  = "pv.kubernetes.io/provisioned-by"
	provisionedPVPrefix = "pvc-"
)

// volumeOutcome is the result of one start-time check of a volume.
type volumeOutcome int

const (
	// volumeGoOn means the next step can run.
	volumeGoOn volumeOutcome = iota
	// volumeWait means the cause may go away: leave the instance pending.
	volumeWait
	// volumeFail means the instance fails with the reason.
	volumeFail
	// volumeLost means the recorded PV no longer exists.
	volumeLost
)

// volumeStep is what a check or the record step decided.
type volumeStep struct {
	Outcome volumeOutcome
	Reason  string
	// Problem is true when the caller stores Reason in the row's problem
	// column.
	Problem bool
	// NeedsPreBind is true when a recorded PV is free but its claimRef
	// doesn't name this instance's claim yet.
	NeedsPreBind bool
	// PV is the PV the step looked at, when it found one.
	PV *corev1.PersistentVolume
	// PVName and SizeBytes are what the caller writes to the row after a
	// successful record.
	PVName    string
	SizeBytes int64
}

func volumeNamespace(instance db.Instance) string { return NamespacePrefix + instance.ID }

// RenderVolumeClaim builds the PersistentVolumeClaim of a volume row in the
// instance's namespace. The row's recorded PV, when it has one, goes in
// volumeName so the claim binds to it.
func RenderVolumeClaim(row db.PersistentVolume, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	meta InstanceMetadata,
) *corev1.PersistentVolumeClaim {
	labels := map[string]string{}
	for key, value := range meta.Labels {
		labels[key] = value
	}

	labels[LabelManagedBy] = ManagedByValue
	labels[LabelInstance] = instance.ID
	labels[LabelTemplate] = template.Name
	labels[LabelVolume] = row.VolumeName

	annotations := map[string]string{}
	for key, value := range meta.Annotations {
		annotations[key] = value
	}

	storageClass := row.StorageClass
	filesystem := corev1.PersistentVolumeFilesystem

	claim := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        row.VolumeName,
			Namespace:   volumeNamespace(instance),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:       &filesystem,
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(row.SizeBytes, resource.BinarySI),
				},
			},
		},
	}

	if row.PvName.Valid {
		claim.Spec.VolumeName = row.PvName.String
	}

	return claim
}

// applyVolumeClaim applies a rendered claim by server-side apply.
func applyVolumeClaim(ctx context.Context, writer client.Client, claim *corev1.PersistentVolumeClaim) error {
	content, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(claim)
	if err != nil {
		return fmt.Errorf("convert PersistentVolumeClaim %s: %w", claim.Name, err)
	}

	// The converter writes the empty status and a null timestamp; the
	// claim's owner of those fields is the API server.
	delete(content, "status")

	if metadata, ok := content["metadata"].(map[string]any); ok {
		delete(metadata, "creationTimestamp")
	}

	err = writer.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: content}),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("apply PersistentVolumeClaim %s/%s: %w", claim.Namespace, claim.Name, err)
	}

	return nil
}

// checkStorageClass checks the StorageClass of a new volume: it must exist
// and keep the PV when the claim goes.
func checkStorageClass(ctx context.Context, reader client.Reader, row db.PersistentVolume) (volumeStep, error) {
	var class storagev1.StorageClass

	err := reader.Get(ctx, types.NamespacedName{Name: row.StorageClass}, &class)
	if apierrors.IsNotFound(err) {
		return volumeStep{
			Outcome: volumeFail,
			Reason:  fmt.Sprintf("StorageClass %s for volume %s doesn't exist.", row.StorageClass, row.VolumeName),
		}, nil
	}

	if err != nil {
		return volumeStep{}, fmt.Errorf("get StorageClass %s: %w", row.StorageClass, err)
	}

	policy := corev1.PersistentVolumeReclaimDelete // the API default
	if class.ReclaimPolicy != nil {
		policy = *class.ReclaimPolicy
	}

	if policy != corev1.PersistentVolumeReclaimRetain {
		return volumeStep{
			Outcome: volumeFail,
			Reason: fmt.Sprintf("StorageClass %s for volume %s has reclaimPolicy %s. Persistent volumes need Retain, "+
				"or the data is deleted when the instance stops.", row.StorageClass, row.VolumeName, policy),
		}, nil
	}

	return volumeStep{Outcome: volumeGoOn}, nil
}

// claimRefNames reports whether the PV's claimRef names the instance's claim
// for the volume.
func claimRefNames(pv *corev1.PersistentVolume, instance db.Instance, volumeName string) bool {
	ref := pv.Spec.ClaimRef

	return ref != nil && ref.Namespace == volumeNamespace(instance) && ref.Name == volumeName
}

// checkRecordedVolume checks the PV a row has recorded before the instance
// uses it.
func checkRecordedVolume(ctx context.Context, reader client.Reader, row db.PersistentVolume,
	instance db.Instance,
) (volumeStep, error) {
	var pv corev1.PersistentVolume

	err := reader.Get(ctx, types.NamespacedName{Name: row.PvName.String}, &pv)
	if apierrors.IsNotFound(err) {
		return volumeStep{Outcome: volumeLost}, nil
	}

	if err != nil {
		return volumeStep{}, fmt.Errorf("get PersistentVolume %s: %w", row.PvName.String, err)
	}

	if pv.Labels[LabelVolumeID] != row.ID || pv.Labels[LabelTemplateUID] != row.TemplateUid ||
		pv.Labels[LabelManagedBy] != ManagedByValue {
		return volumeStep{
			Outcome: volumeFail, Problem: true, PV: &pv,
			Reason: fmt.Sprintf("PersistentVolume %s doesn't carry toolgate's labels for volume %s, so toolgate won't use it.",
				pv.Name, row.VolumeName),
		}, nil
	}

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return volumeStep{
			Outcome: volumeFail, Problem: true, PV: &pv,
			Reason: fmt.Sprintf("PersistentVolume %s has reclaim policy %s, so toolgate won't bind it.",
				pv.Name, pv.Spec.PersistentVolumeReclaimPolicy),
		}, nil
	}

	switch pv.Status.Phase {
	case corev1.VolumeBound:
		if claimRefNames(&pv, instance, row.VolumeName) {
			return volumeStep{Outcome: volumeGoOn, PV: &pv}, nil
		}

		other := "another claim"
		if pv.Spec.ClaimRef != nil {
			other = pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
		}

		return volumeStep{
			Outcome: volumeWait, PV: &pv,
			Reason: fmt.Sprintf("Volume %s is still bound to %s.", row.VolumeName, other),
		}, nil
	case corev1.VolumeReleased, corev1.VolumeAvailable:
		return volumeStep{Outcome: volumeGoOn, PV: &pv, NeedsPreBind: !claimRefNames(&pv, instance, row.VolumeName)}, nil
	default:
		return volumeStep{
			Outcome: volumeFail, PV: &pv,
			Reason: fmt.Sprintf("PersistentVolume %s is %s: %s", pv.Name, pv.Status.Phase, pv.Status.Message),
		}, nil
	}
}

// preBind points a free PV's claimRef at the instance's claim. The merge
// patch carries the resourceVersion, so a change in between fails with a
// conflict, which the caller treats as "try again next pass". The new
// claimRef has no uid and no resourceVersion, so the patch sets both to
// null: a merge patch would otherwise keep the old claim's values.
func preBind(ctx context.Context, writer client.Client, pv *corev1.PersistentVolume, instance db.Instance,
	volumeName string,
) error {
	old := pv.DeepCopy()
	pv = pv.DeepCopy()
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Namespace:  volumeNamespace(instance),
		Name:       volumeName,
	}

	patch := client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{})

	if err := writer.Patch(ctx, pv, patch); err != nil {
		return fmt.Errorf("pre-bind PersistentVolume %s: %w", pv.Name, err)
	}

	return nil
}

// recordVolume checks the PV that backs a new volume's claim and, when it is
// the one a provisioner made for that claim, labels it. The caller writes
// the returned PVName and SizeBytes to the row with a compare-and-set.
func recordVolume(ctx context.Context, reader client.Reader, writer client.Client, row db.PersistentVolume,
	instance db.Instance, template *v1alpha1.MCPServerTemplate,
) (volumeStep, error) {
	var claim corev1.PersistentVolumeClaim

	err := reader.Get(ctx, types.NamespacedName{Namespace: volumeNamespace(instance), Name: row.VolumeName}, &claim)
	if apierrors.IsNotFound(err) {
		return volumeStep{Outcome: volumeWait, Reason: fmt.Sprintf("Volume %s has no claim yet.", row.VolumeName)}, nil
	}

	if err != nil {
		return volumeStep{}, fmt.Errorf("get PersistentVolumeClaim %s: %w", row.VolumeName, err)
	}

	if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
		return volumeStep{Outcome: volumeWait, Reason: fmt.Sprintf("Volume %s isn't bound yet.", row.VolumeName)}, nil
	}

	var pv corev1.PersistentVolume

	err = reader.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, &pv)
	if apierrors.IsNotFound(err) {
		return volumeStep{Outcome: volumeWait, Reason: fmt.Sprintf("Volume %s isn't bound yet.", row.VolumeName)}, nil
	}

	if err != nil {
		return volumeStep{}, fmt.Errorf("get PersistentVolume %s: %w", claim.Spec.VolumeName, err)
	}

	_, provisioned := pv.Annotations[annotationPVProvBy]
	if pv.Name != provisionedPVPrefix+string(claim.UID) || pv.Spec.ClaimRef == nil ||
		pv.Spec.ClaimRef.UID != claim.UID || !provisioned {
		return volumeStep{
			Outcome: volumeFail, Problem: true, PV: &pv,
			Reason: fmt.Sprintf("PersistentVolume %s wasn't provisioned for volume %s.", pv.Name, row.VolumeName),
		}, nil
	}

	if owner, found := pv.Labels[LabelVolumeID]; found && owner != row.ID {
		return volumeStep{
			Outcome: volumeFail, Problem: true, PV: &pv,
			Reason: fmt.Sprintf("PersistentVolume %s belongs to another volume.", pv.Name),
		}, nil
	}

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return volumeStep{
			Outcome: volumeFail, Problem: true, PV: &pv,
			Reason: fmt.Sprintf("PersistentVolume %s has reclaim policy %s, so toolgate won't bind it.",
				pv.Name, pv.Spec.PersistentVolumeReclaimPolicy),
		}, nil
	}

	old := pv.DeepCopy()
	if pv.Labels == nil {
		pv.Labels = map[string]string{}
	}

	pv.Labels[LabelManagedBy] = ManagedByValue
	pv.Labels[LabelTemplate] = template.Name
	pv.Labels[LabelTemplateUID] = row.TemplateUid
	pv.Labels[LabelVolume] = row.VolumeName
	pv.Labels[LabelVolumeID] = row.ID

	patch := client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{})
	if err := writer.Patch(ctx, &pv, patch); err != nil {
		return volumeStep{}, fmt.Errorf("label PersistentVolume %s: %w", pv.Name, err)
	}

	capacity := pv.Spec.Capacity[corev1.ResourceStorage]

	return volumeStep{Outcome: volumeGoOn, PV: &pv, PVName: pv.Name, SizeBytes: capacity.Value()}, nil
}
