package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// How long the clean-up waits before it acts on a PV itself.
const (
	// VolumeReclaimWait is how long a PV with the Delete reclaim policy may
	// take to go away before the clean-up deletes the object itself.
	VolumeReclaimWait = 10 * time.Minute
	// VolumeForgetWait is how long a deletion may take before the row is
	// forgotten.
	VolumeForgetWait = time.Hour
)

// Delete reasons of a persistent volume row.
const (
	volumeDeleteTemplateDeleted = "template-deleted"
	volumeDeleteUnused          = "unused"
)

// VolumeCleaner decides which persistent volumes to delete and deletes their
// PVs. Only the leader runs it, on its full passes.
type VolumeCleaner struct {
	Pool *pgxpool.Pool
	// Reader reads PVs without a cache; Writer patches and deletes them.
	Reader client.Reader
	Writer client.Client
	// Template returns the last valid generation of a template. An invalid
	// template still has one, so it counts as present.
	Template func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool)
	// Enabled is the gateway setting. With it off, nothing runs.
	Enabled bool
	// UnusedRetention is how long a free volume may sit unused. Zero never
	// deletes unused volumes.
	UnusedRetention time.Duration
	Metrics         *Metrics
	AuditOutput     io.Writer
	Log             *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Pass wraps a full pass so that the volume clean-up runs before it.
func (c *VolumeCleaner) Pass(pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := c.Run(ctx); err != nil {
			return errors.Join(err, pass(ctx))
		}

		return pass(ctx)
	}
}

func (c *VolumeCleaner) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}

	return time.Now()
}

func (c *VolumeCleaner) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}

	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Run decides deletions, deletes what is decided, and sets the gauges.
func (c *VolumeCleaner) Run(ctx context.Context) error {
	if !c.Enabled {
		return nil
	}

	queries := db.New(c.Pool)

	var problems []error

	if err := c.decide(ctx, queries); err != nil {
		problems = append(problems, err)
	}

	rows, err := queries.ListPersistentVolumesToDelete(ctx)
	if err != nil {
		problems = append(problems, fmt.Errorf("list volumes to delete: %w", err))
	}

	for _, row := range rows {
		if err := c.deleteOne(ctx, queries, row); err != nil {
			problems = append(problems, fmt.Errorf("delete volume %s: %w", row.ID, err))
		}
	}

	if err := c.setGauges(ctx, queries); err != nil {
		problems = append(problems, err)
	}

	return errors.Join(problems...)
}

// decide marks the free rows whose template is gone and, when retention is
// on, the ones unused for too long.
func (c *VolumeCleaner) decide(ctx context.Context, queries *db.Queries) error {
	idle, err := queries.ListIdlePersistentVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list idle volumes: %w", err)
	}

	var problems []error

	for _, row := range idle {
		template, found := c.Template(types.NamespacedName{Namespace: row.TemplateNs, Name: row.TemplateName})
		if found && string(template.UID) == row.TemplateUid {
			continue
		}

		if err := c.mark(ctx, row, volumeDeleteTemplateDeleted, func(queries *db.Queries) (int64, error) {
			return queries.MarkPersistentVolumeForDeletion(ctx, db.MarkPersistentVolumeForDeletionParams{
				ID: row.ID, DeleteReason: volumeDeleteTemplateDeleted,
			})
		}); err != nil {
			problems = append(problems, err)
		}
	}

	if c.UnusedRetention <= 0 {
		return errors.Join(problems...)
	}

	cutoff := pgtype.Timestamptz{Time: c.now().Add(-c.UnusedRetention), Valid: true}

	unused, err := queries.ListUnusedPersistentVolumes(ctx, cutoff)
	if err != nil {
		return errors.Join(append(problems, fmt.Errorf("list unused volumes: %w", err))...)
	}

	for _, row := range unused {
		if err := c.mark(ctx, row, volumeDeleteUnused, func(queries *db.Queries) (int64, error) {
			return queries.MarkPersistentVolumeUnused(ctx, db.MarkPersistentVolumeUnusedParams{ID: row.ID, Cutoff: cutoff})
		}); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// mark runs the marking update and writes volume.delete in one transaction.
// The update changes nothing when a start took the row in between.
func (c *VolumeCleaner) mark(ctx context.Context, row db.PersistentVolume, reason string,
	update func(*db.Queries) (int64, error),
) error {
	err := pgx.BeginFunc(ctx, c.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		changed, err := update(queries)
		if err != nil || changed == 0 {
			return err
		}

		out := c.AuditOutput
		if out == nil {
			out = io.Discard
		}

		return audit.NewRecorder(out, queries).Record(ctx, audit.Event{
			Type:      audit.TypeVolumeDelete,
			Server:    row.TemplateName,
			Principal: row.ScopeID,
			Details: map[string]any{
				"volume":           row.VolumeName,
				"persistentVolume": row.PvName.String,
				"reason":           reason,
				"owner":            row.ScopeID,
			},
		})
	})
	if err != nil {
		return fmt.Errorf("mark volume %s for deletion: %w", row.ID, err)
	}

	return nil
}

// setProblem stores why the PV isn't deleted yet, for the admin list.
func (c *VolumeCleaner) setProblem(ctx context.Context, queries *db.Queries, row db.PersistentVolume, problem string) error {
	if row.Problem == problem {
		return nil
	}

	err := queries.SetPersistentVolumeProblem(ctx, db.SetPersistentVolumeProblemParams{
		ID: row.ID, Problem: truncate(problem, store.MaxPersistentVolumeProblemBytes),
	})
	if err != nil {
		return fmt.Errorf("set problem of volume %s: %w", row.ID, err)
	}

	return nil
}

// dropRow deletes the row while its deletion is still decided and nothing
// holds it. It reports whether the row went.
func (c *VolumeCleaner) dropRow(ctx context.Context, queries *db.Queries, row db.PersistentVolume) (bool, error) {
	changed, err := queries.DeleteDecidedPersistentVolume(ctx, row.ID)
	if err != nil {
		return false, fmt.Errorf("delete row of volume %s: %w", row.ID, err)
	}

	return changed > 0, nil
}

func (c *VolumeCleaner) deleteOne(ctx context.Context, queries *db.Queries, row db.PersistentVolume) error {
	if !row.PvName.Valid {
		_, err := c.dropRow(ctx, queries, row)

		return err
	}

	var pv corev1.PersistentVolume

	err := c.Reader.Get(ctx, types.NamespacedName{Name: row.PvName.String}, &pv)
	if apierrors.IsNotFound(err) {
		gone, err := c.dropRow(ctx, queries, row)
		if err == nil && gone && row.ReclaimPatchedAt.Valid {
			c.Metrics.ObserveVolumeEvent(VolumeEventDeleted)
		}

		return err
	}

	if err != nil {
		return fmt.Errorf("get PersistentVolume %s: %w", row.PvName.String, err)
	}

	if pv.Labels[LabelVolumeID] != row.ID || pv.Labels[LabelTemplateUID] != row.TemplateUid ||
		pv.Labels[LabelManagedBy] != ManagedByValue {
		return c.refuseForeignPV(ctx, queries, row, pv.Name)
	}

	if c.forgetDue(row) {
		return c.forget(ctx, queries, row)
	}

	if pv.Status.Phase == corev1.VolumeBound {
		return c.setProblem(ctx, queries, row, fmt.Sprintf(
			"PersistentVolume %s is still bound to a claim that isn't toolgate's live claim.", pv.Name))
	}

	row, proceed, err := c.markReclaim(ctx, queries, row)
	if err != nil || !proceed {
		return err
	}

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		old := pv.DeepCopy()
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete

		patch := client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{})
		if err := c.Writer.Patch(ctx, &pv, patch); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return nil
			}

			return fmt.Errorf("set reclaim policy of PersistentVolume %s: %w", pv.Name, err)
		}
	}

	waited := c.now().Sub(row.ReclaimPatchedAt.Time) > testtime.Scale(VolumeReclaimWait)
	if pv.DeletionTimestamp != nil || (pv.Status.Phase != corev1.VolumeAvailable && !waited) {
		return nil
	}

	uid := pv.UID

	err = c.Writer.Delete(ctx, &pv, client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete PersistentVolume %s: %w", pv.Name, err)
	}

	return nil
}

// forgetDue reports whether the deletion has run past VolumeForgetWait,
// counted from the later of the decision and the last release.
func (c *VolumeCleaner) forgetDue(row db.PersistentVolume) bool {
	from := row.LastUsedAt.Time
	if row.DeleteRequestedAt.Valid && row.DeleteRequestedAt.Time.After(from) {
		from = row.DeleteRequestedAt.Time
	}

	return c.now().Sub(from) > testtime.Scale(VolumeForgetWait)
}

func (c *VolumeCleaner) forget(ctx context.Context, queries *db.Queries, row db.PersistentVolume) error {
	gone, err := c.dropRow(ctx, queries, row)
	if err != nil || !gone {
		return err
	}

	c.log().Warn("forgot a persistent volume whose deletion never finished",
		"persistentVolume", row.PvName.String, "volume", row.ID, "problem", row.Problem)
	c.Metrics.ObserveVolumeEvent(VolumeEventDeleteFailed)

	return nil
}

// refuseForeignPV forgets a row whose PV isn't toolgate's. The PV stays as it
// is.
func (c *VolumeCleaner) refuseForeignPV(ctx context.Context, queries *db.Queries, row db.PersistentVolume, pvName string) error {
	problem := fmt.Sprintf("PersistentVolume %s doesn't carry toolgate's labels for this volume, so toolgate didn't delete it.", pvName)

	if err := c.setProblem(ctx, queries, row, problem); err != nil {
		return err
	}

	gone, err := c.dropRow(ctx, queries, row)
	if err != nil || !gone {
		return err
	}

	c.log().Warn("didn't delete a PersistentVolume without toolgate's labels", "persistentVolume", pvName, "volume", row.ID)
	c.Metrics.ObserveVolumeEvent(VolumeEventDeleteFailed)

	return nil
}

// markReclaim records, under the row lock, that the PV is about to be
// touched. It reports false when a start cleared the mark or holds the
// volume. The row it returns has the stored reclaim_patched_at.
func (c *VolumeCleaner) markReclaim(ctx context.Context, queries *db.Queries, row db.PersistentVolume) (db.PersistentVolume, bool, error) {
	proceed := false

	err := pgx.BeginFunc(ctx, c.Pool, func(tx pgx.Tx) error {
		txQueries := db.New(tx)

		locked, err := txQueries.LockPersistentVolumeByID(ctx, row.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return err
		}

		if locked.DeleteReason == "" || locked.InstanceID.Valid {
			return nil
		}

		if !locked.ReclaimPatchedAt.Valid {
			if _, err := txQueries.SetPersistentVolumeReclaimPatched(ctx, row.ID); err != nil {
				return err
			}
		}

		proceed = true

		return nil
	})
	if err != nil {
		return row, false, fmt.Errorf("record reclaim of volume %s: %w", row.ID, err)
	}

	if !proceed {
		return row, false, nil
	}

	current, err := queries.GetPersistentVolume(ctx, row.ID)
	if err != nil {
		return row, false, fmt.Errorf("get volume %s: %w", row.ID, err)
	}

	return current, true, nil
}

// setGauges sets the persistent volume gauges. Every series is set, so a
// state that emptied shows 0.
func (c *VolumeCleaner) setGauges(ctx context.Context, queries *db.Queries) error {
	if c.Metrics == nil {
		return nil
	}

	counts, err := queries.CountPersistentVolumes(ctx)
	if err != nil {
		return fmt.Errorf("count volumes: %w", err)
	}

	volumes := map[[2]string]float64{}
	bytes := map[string]float64{}

	for _, scope := range []string{"per-template", "per-user"} {
		bytes[scope] = 0

		for _, state := range []string{"in_use", "idle", "deleting"} {
			volumes[[2]string{scope, state}] = 0
		}
	}

	for _, count := range counts {
		volumes[[2]string{count.Scope, count.State}] = float64(count.Volumes)
		bytes[count.Scope] += float64(count.Bytes)
	}

	for key, value := range volumes {
		c.Metrics.PersistentVolumes.WithLabelValues(key[0], key[1]).Set(value)
	}

	for scope, value := range bytes {
		c.Metrics.PersistentVolumeBytes.WithLabelValues(scope).Set(value)
	}

	return nil
}
