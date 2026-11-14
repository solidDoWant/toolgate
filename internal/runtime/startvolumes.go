package runtime

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

// claimedVolume is a volume an instance holds and has checked, ready to be
// bound once the namespace exists.
type claimedVolume struct {
	Row db.PersistentVolume
	// PV is the recorded PV to pre-bind, when NeedsPreBind is true.
	PV           *corev1.PersistentVolume
	NeedsPreBind bool
}

// volumeReader is the reader of StorageClasses, PVs and PVCs: the uncached
// API reader, so a check never sees an old object.
func (s *Starter) volumeReader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}

	return s.Client
}

// volumeClaims returns the template's volumes as claims.
func volumeClaims(template *v1alpha1.MCPServerTemplate) ([]VolumeClaim, error) {
	claims := make([]VolumeClaim, 0, len(template.Spec.PersistentVolumes))

	for _, volume := range template.Spec.PersistentVolumes {
		size, err := resource.ParseQuantity(volume.Size)
		if err != nil {
			return nil, fmt.Errorf("size of volume %s: %w", volume.Name, err)
		}

		claims = append(claims, VolumeClaim{
			Name:         volume.Name,
			Scope:        templatescope.EffectiveVolumeScope(template.Spec, volume),
			StorageClass: volume.StorageClassName,
			SizeBytes:    size.Value(),
		})
	}

	return claims, nil
}

// claimVolumes claims the template's volumes and checks them before the
// render. It reports false when the row waits or has failed.
func (s *Starter) claimVolumes(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
) ([]claimedVolume, bool, error) {
	if !s.PersistentVolumesEnabled || s.Volumes == nil {
		return nil, false, s.fail(ctx, instance, StatusPending, FeatureOffReason)
	}

	claims, err := volumeClaims(template)
	if err != nil {
		return nil, false, s.fail(ctx, instance, StatusPending, "The template didn't render: "+err.Error())
	}

	result, err := s.Volumes.ClaimVolumes(ctx, instance, template.Name, claims, s.VolumeLimitPerUser)
	if err != nil || result.Refused {
		return nil, false, err
	}

	if result.Wait != "" {
		return nil, false, s.waitForVolume(ctx, instance, template, result.WaitVolume, result.Wait)
	}

	claimed := make([]claimedVolume, 0, len(result.Rows))

	for _, row := range result.Rows {
		volume, proceed, err := s.checkClaimedVolume(ctx, instance, template, row)
		if err != nil || !proceed {
			return nil, false, err
		}

		claimed = append(claimed, volume)
	}

	return claimed, true, nil
}

// checkClaimedVolume checks the StorageClass of a new volume, or the PV of a
// recorded one. A recorded PV that is gone makes the volume new again.
func (s *Starter) checkClaimedVolume(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	row db.PersistentVolume,
) (claimedVolume, bool, error) {
	reader := s.volumeReader()

	if row.PvName.Valid {
		step, err := checkRecordedVolume(ctx, reader, row, instance)
		if err != nil {
			return claimedVolume{}, false, err
		}

		if step.Outcome != volumeLost {
			proceed, err := s.volumeOutcome(ctx, instance, template, row, step)

			return claimedVolume{Row: row, PV: step.PV, NeedsPreBind: step.NeedsPreBind}, proceed, err
		}

		cleared, err := s.Volumes.LoseVolume(ctx, instance, row)
		if err != nil {
			return claimedVolume{}, false, err
		}

		if !cleared {
			// The row changed in between: the next pass reads it again.
			return claimedVolume{}, false, nil
		}

		row.PvName.Valid, row.PvName.String = false, ""
	}

	step, err := checkStorageClass(ctx, reader, row)
	if err != nil {
		return claimedVolume{}, false, err
	}

	proceed, err := s.volumeOutcome(ctx, instance, template, row, step)

	return claimedVolume{Row: row}, proceed, err
}

// volumeOutcome acts on a start-time check: it fails the instance, or
// leaves it waiting, or reports true to go on.
func (s *Starter) volumeOutcome(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	row db.PersistentVolume, step volumeStep,
) (bool, error) {
	switch step.Outcome {
	case volumeGoOn:
		return true, nil
	case volumeWait:
		return false, s.waitForVolume(ctx, instance, template, row.VolumeName, step.Reason)
	default:
		if step.Problem {
			if err := s.Volumes.SetVolumeProblem(ctx, row.ID, step.Reason); err != nil {
				return false, err
			}
		}

		return false, s.fail(ctx, instance, StatusPending, step.Reason)
	}
}

// waitForVolume leaves a pending row waiting with the reason, or fails it
// once startTimeout has passed since the attempt started.
func (s *Starter) waitForVolume(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	volume, reason string,
) error {
	if s.timedOut(instance, template) {
		return s.fail(ctx, instance, StatusPending, fmt.Sprintf("Timed out waiting for volume %s: %s", volume, reason))
	}

	return s.Volumes.SetWaitReason(ctx, instance.ID, reason)
}

// bindVolumes prepares the namespace, pre-binds each recorded PV that
// doesn't name the instance's claim yet, and applies the claims. It reports
// false when the step must run again on the next pass.
func (s *Starter) bindVolumes(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	volumes []claimedVolume, meta InstanceMetadata,
) (bool, error) {
	prepared, err := s.prepareNamespace(ctx, instance, template, meta)
	if err != nil || !prepared {
		return false, err
	}

	for _, volume := range volumes {
		if !volume.NeedsPreBind || volume.PV == nil {
			continue
		}

		err := preBind(ctx, s.Client, volume.PV, instance, volume.Row.VolumeName)
		if apierrors.IsConflict(errors.Unwrap(err)) || apierrors.IsConflict(err) {
			// The PV changed since the check: the next pass checks it again.
			return false, nil
		}

		if err != nil {
			return false, err
		}
	}

	for _, volume := range volumes {
		if err := applyVolumeClaim(ctx, s.Client, RenderVolumeClaim(volume.Row, instance, template, meta)); err != nil {
			return false, err
		}
	}

	return true, nil
}

// recordHeldVolumes records the PV of each held row that has none yet. It
// reports true when every held row is recorded.
func (s *Starter) recordHeldVolumes(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
) (bool, error) {
	if s.Volumes == nil {
		return true, nil
	}

	rows, err := s.Volumes.HeldVolumes(ctx, instance.ID)
	if err != nil {
		return false, err
	}

	all := true

	for _, row := range rows {
		if row.PvName.Valid {
			continue
		}

		step, err := recordVolume(ctx, s.volumeReader(), s.Client, row, instance, template)
		if err != nil {
			return false, err
		}

		switch step.Outcome {
		case volumeGoOn:
			recorded, err := s.Volumes.RecordVolume(ctx, instance, row, step.PVName, step.SizeBytes)
			if err != nil {
				return false, err
			}

			all = all && recorded
		case volumeWait:
			all = false
		default:
			if step.Problem {
				if err := s.Volumes.SetVolumeProblem(ctx, row.ID, step.Reason); err != nil {
					return false, err
				}
			}

			return false, s.fail(ctx, instance, StatusStarting, step.Reason)
		}
	}

	return all, nil
}
