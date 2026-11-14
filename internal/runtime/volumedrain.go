package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// VolumeDrainTimeout is how long, counted from the start of the stop, teardown
// waits for the pods that mount an instance's volumes before it deletes the
// namespace anyway.
const VolumeDrainTimeout = 2 * time.Minute

const reasonDrainingVolumes = "Waiting for pods to stop before the namespace is deleted."

var scaleToZero = client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`))

// drainVolumes runs the steps that come before the namespace of an instance
// that holds volumes is deleted: it records the PVs that aren't recorded yet,
// scales the controllers to zero and waits for the pods that mount a held
// claim. It returns true when the namespace may be deleted.
func (t *Teardown) drainVolumes(ctx context.Context, instance db.Instance) (bool, error) {
	if !t.VolumesEnabled || t.Writer == nil {
		return true, nil
	}

	held, err := db.New(t.Pool).ListPersistentVolumesHeldBy(ctx, pgtype.Text{String: instance.ID, Valid: true})
	if err != nil {
		return false, fmt.Errorf("instance %s: list held volumes: %w", instance.ID, err)
	}

	if len(held) == 0 {
		return true, nil
	}

	t.recordHeldVolumes(ctx, instance, held)

	claims := make(map[string]bool, len(held))
	for _, row := range held {
		claims[row.VolumeName] = true
	}

	expired := !instance.StoppingAt.Valid || t.now().Sub(instance.StoppingAt.Time) >= testtime.Scale(VolumeDrainTimeout)

	if !expired {
		if err := t.scaleControllersToZero(ctx, instance.Namespace); err != nil {
			return false, err
		}
	}

	mounting, err := t.podsMounting(ctx, instance.Namespace, claims)
	if err != nil {
		return false, err
	}

	switch {
	case mounting == 0:
		return true, nil
	case !expired:
		return false, t.setReason(ctx, instance, reasonDrainingVolumes)
	}

	t.Log.WarnContext(ctx, "volume drain timed out; deleting the namespace with pods still running",
		slog.String("instance", instance.ID), slog.String("namespace", instance.Namespace), slog.Int("pods", mounting))
	t.Metrics.ObserveVolumeEvent(VolumeEventDrainTimeout)

	return true, nil
}

// recordHeldVolumes records the PV of each held row that has none. A failure
// goes to the row's problem and never fails the instance, which is stopping.
func (t *Teardown) recordHeldVolumes(ctx context.Context, instance db.Instance, held []db.PersistentVolume) {
	queries := db.New(t.Pool)
	template := &v1alpha1.MCPServerTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: instance.TemplateNs, Name: instance.TemplateName}}

	for _, row := range held {
		if row.PvName.Valid {
			continue
		}

		step, err := recordVolume(ctx, t.Reader, t.Writer, row, instance, template)
		if err != nil {
			t.Log.WarnContext(ctx, "record volume at stop failed", slog.String("instance", instance.ID),
				slog.String("volume", row.VolumeName), slog.Any("error", err))

			continue
		}

		switch {
		case step.Outcome == volumeGoOn:
			_, err = queries.RecordPersistentVolume(ctx, db.RecordPersistentVolumeParams{
				ID: row.ID, PvName: pgtype.Text{String: step.PVName, Valid: true}, SizeBytes: step.SizeBytes,
				InstanceID: pgtype.Text{String: instance.ID, Valid: true},
			})
		case step.Problem:
			err = queries.SetPersistentVolumeProblem(ctx, db.SetPersistentVolumeProblemParams{ID: row.ID, Problem: step.Reason})
		}

		if err != nil {
			t.Log.WarnContext(ctx, "store volume at stop failed", slog.String("instance", instance.ID),
				slog.String("volume", row.VolumeName), slog.Any("error", err))
		}
	}
}

// scaleControllersToZero sets spec.replicas to 0 on every Deployment and
// StatefulSet in the namespace that doesn't have 0 already.
func (t *Teardown) scaleControllersToZero(ctx context.Context, namespace string) error {
	var deployments appsv1.DeploymentList
	if err := t.Reader.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list Deployments in %s: %w", namespace, err)
	}

	for index := range deployments.Items {
		deployment := &deployments.Items[index]
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
			continue
		}

		if err := t.Writer.Patch(ctx, deployment, scaleToZero); err != nil {
			return fmt.Errorf("scale Deployment %s/%s to zero: %w", namespace, deployment.Name, err)
		}
	}

	var sets appsv1.StatefulSetList
	if err := t.Reader.List(ctx, &sets, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list StatefulSets in %s: %w", namespace, err)
	}

	for index := range sets.Items {
		set := &sets.Items[index]
		if set.Spec.Replicas != nil && *set.Spec.Replicas == 0 {
			continue
		}

		if err := t.Writer.Patch(ctx, set, scaleToZero); err != nil {
			return fmt.Errorf("scale StatefulSet %s/%s to zero: %w", namespace, set.Name, err)
		}
	}

	return nil
}

// podsMounting counts the pods in the namespace that aren't finished and
// mount one of the claims.
func (t *Teardown) podsMounting(ctx context.Context, namespace string, claims map[string]bool) (int, error) {
	var pods corev1.PodList
	if err := t.Reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("list Pods in %s: %w", namespace, err)
	}

	count := 0

	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && claims[volume.PersistentVolumeClaim.ClaimName] {
				count++

				break
			}
		}
	}

	return count, nil
}
