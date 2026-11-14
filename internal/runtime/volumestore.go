package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

// VolumeClaim is one persistent volume an instance's template asks for.
type VolumeClaim struct {
	Name         string
	Scope        string
	StorageClass string
	SizeBytes    int64
}

// VolumeClaimResult is the outcome of a claim. At most one of Wait and
// Refused is set; with neither, Rows holds every claimed row.
type VolumeClaimResult struct {
	// Rows are the claimed rows in volume-name order.
	Rows []db.PersistentVolume
	// WaitVolume and Wait name the volume another holder or a deletion
	// keeps, and why. Nothing was claimed.
	WaitVolume string
	Wait       string
	// Refused is true when a new per-user volume would pass the user's
	// size limit. The instance has failed with that reason.
	Refused bool
}

// VolumeStore is what the start path reads and writes about persistent
// volumes in PostgreSQL.
type VolumeStore interface {
	// ClaimVolumes claims every volume for a pending instance in one
	// transaction, or none. server names the template in the size limit's
	// reason; limitPerUser is the per-user byte limit, 0 for none.
	ClaimVolumes(ctx context.Context, instance db.Instance, server string, claims []VolumeClaim,
		limitPerUser int64) (VolumeClaimResult, error)
	// LoseVolume clears the recorded PV of a row whose PV no longer exists,
	// and records volume.lost. It reports false when the row changed in
	// between.
	LoseVolume(ctx context.Context, instance db.Instance, row db.PersistentVolume) (bool, error)
	// RecordVolume writes the PV of a new volume held by the instance and
	// records volume.create. It reports false when the row is already
	// recorded or no longer held by the instance.
	RecordVolume(ctx context.Context, instance db.Instance, row db.PersistentVolume, pvName string,
		sizeBytes int64) (bool, error)
	// SetVolumeProblem stores what is wrong with a volume.
	SetVolumeProblem(ctx context.Context, id, problem string) error
	// SetWaitReason sets the reason of a pending instance that waits.
	SetWaitReason(ctx context.Context, id, reason string) error
	// HeldVolumes returns the rows an instance holds, in volume-name order.
	HeldVolumes(ctx context.Context, instanceID string) ([]db.PersistentVolume, error)
}

// FeatureOffReason is the failure reason of a start of a template with
// persistent volumes while the gateway setting is off.
const FeatureOffReason = "persistentVolumes needs the gateway setting persistentVolumes.enabled"

// VolumeLimitReason is the failure reason of a start whose new per-user
// volume would pass the user's size limit.
func VolumeLimitReason(server, volume string, sizeBytes, usedBytes, limitBytes int64) string {
	return fmt.Sprintf("%s needs a %s volume %s, but your volumes already use %s of your %s limit. "+
		"Ask an admin to delete a volume you no longer need (toolgate admin volumes).",
		server, formatBytes(sizeBytes), volume, formatBytes(usedBytes), formatBytes(limitBytes))
}

func formatBytes(value int64) string {
	return resource.NewQuantity(value, resource.BinarySI).String()
}

// volumeScopeID is the scope ID of a volume of the instance: the owner for a
// per-user volume, empty for a per-template one.
func volumeScopeID(instance db.Instance, scope string) string {
	if scope == templatescope.VolumePerUser {
		return instance.OwnerPrincipal
	}

	return ""
}

// errClaimStopped ends a claim transaction without a commit.
var errClaimStopped = errors.New("claim stopped")

func (s PostgresStartStore) auditOutput() io.Writer {
	if s.AuditOutput == nil {
		return io.Discard
	}

	return s.AuditOutput
}

// ClaimVolumes implements VolumeStore. The rows are locked in volume-name
// order, so two claims can't deadlock.
func (s PostgresStartStore) ClaimVolumes(ctx context.Context, instance db.Instance, server string, claims []VolumeClaim,
	limitPerUser int64,
) (VolumeClaimResult, error) {
	var (
		result  VolumeClaimResult
		rebound int
		refusal string
	)

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)
		rows := make([]db.PersistentVolume, 0, len(claims))
		rebound = 0

		for _, claim := range sortedClaims(claims) {
			row, wait, refused, took, err := s.claimOne(ctx, tx, queries, instance, server, claim, limitPerUser)
			if err != nil {
				return err
			}

			if wait != "" {
				result = VolumeClaimResult{WaitVolume: claim.Name, Wait: wait}

				return errClaimStopped
			}

			if refused != "" {
				refusal = refused

				return errClaimStopped
			}

			if took && row.PvName.Valid {
				rebound++
			}

			rows = append(rows, row)
		}

		result = VolumeClaimResult{Rows: rows}

		return nil
	})
	if errors.Is(err, errClaimStopped) {
		err = nil
	}

	if err != nil {
		return VolumeClaimResult{}, fmt.Errorf("claim volumes of instance %s: %w", instance.ID, err)
	}

	if refusal != "" {
		if err := s.failForLimit(ctx, instance, refusal); err != nil {
			return VolumeClaimResult{}, err
		}

		return VolumeClaimResult{Refused: true}, nil
	}

	for range rebound {
		s.Metrics.ObserveVolumeEvent(VolumeEventRebound)
	}

	return result, nil
}

// claimOne claims one volume inside the claim transaction. It returns the
// row, or a wait reason, or a refusal reason. took is true when this call
// made the instance the holder.
func (s PostgresStartStore) claimOne(ctx context.Context, tx pgx.Tx, queries *db.Queries, instance db.Instance,
	server string, claim VolumeClaim, limitPerUser int64,
) (db.PersistentVolume, string, string, bool, error) {
	scopeID := volumeScopeID(instance, claim.Scope)

	var generated [16]byte
	if _, err := rand.Read(generated[:]); err != nil {
		return db.PersistentVolume{}, "", "", false, fmt.Errorf("volume ID: %w", err)
	}

	inserted, err := queries.InsertPersistentVolume(ctx, db.InsertPersistentVolumeParams{
		ID: hex.EncodeToString(generated[:]), TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName,
		TemplateUid: instance.TemplateUid, VolumeName: claim.Name, Scope: claim.Scope, ScopeID: scopeID,
		StorageClass: claim.StorageClass, SizeBytes: claim.SizeBytes,
	})
	if err != nil {
		return db.PersistentVolume{}, "", "", false, fmt.Errorf("insert volume %s: %w", claim.Name, err)
	}

	if inserted > 0 && claim.Scope == templatescope.VolumePerUser && limitPerUser > 0 {
		// Two starts of one user lock the same key, so they can't both pass
		// the limit. The sum then sees every committed row of the user.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('toolgate-volume-limit:' || $1))", scopeID); err != nil {
			return db.PersistentVolume{}, "", "", false, fmt.Errorf("lock volume limit: %w", err)
		}

		total, err := queries.SumPersistentVolumeBytesForUser(ctx, scopeID)
		if err != nil {
			return db.PersistentVolume{}, "", "", false, fmt.Errorf("sum volumes: %w", err)
		}

		if total > limitPerUser {
			return db.PersistentVolume{}, "",
				VolumeLimitReason(server, claim.Name, claim.SizeBytes, total-claim.SizeBytes, limitPerUser), false, nil
		}
	}

	row, err := queries.LockPersistentVolume(ctx, db.LockPersistentVolumeParams{
		TemplateUid: instance.TemplateUid, VolumeName: claim.Name, ScopeID: scopeID,
	})
	if err != nil {
		return db.PersistentVolume{}, "", "", false, fmt.Errorf("lock volume %s: %w", claim.Name, err)
	}

	if row.DeleteReason != "" {
		if row.DeleteReason != "unused" || row.ReclaimPatchedAt.Valid {
			return row, fmt.Sprintf("Volume %s is being deleted.", claim.Name), "", false, nil
		}

		// The clean-up hasn't touched the PV yet: the user who came back
		// keeps the data.
		if _, err := queries.ClearPersistentVolumeUnusedMark(ctx, row.ID); err != nil {
			return db.PersistentVolume{}, "", "", false, fmt.Errorf("clear unused mark of volume %s: %w", claim.Name, err)
		}

		row.DeleteReason, row.DeleteRequestedAt, row.Problem = "", pgtype.Timestamptz{}, ""
	}

	if row.InstanceID.Valid && row.InstanceID.String == instance.ID {
		return row, "", "", false, nil
	}

	if row.InstanceID.Valid {
		holder, err := queries.GetInstance(ctx, row.InstanceID.String)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return db.PersistentVolume{}, "", "", false, fmt.Errorf("get holder of volume %s: %w", claim.Name, err)
		}

		if err == nil && holder.Status != "stopped" {
			return row, fmt.Sprintf("Volume %s is still held by instance %s, which is %s.",
				claim.Name, holder.ID, holder.Status), "", false, nil
		}
	}

	if _, err := queries.SetPersistentVolumeHolder(ctx, db.SetPersistentVolumeHolderParams{
		ID: row.ID, InstanceID: pgtype.Text{String: instance.ID, Valid: true},
	}); err != nil {
		return db.PersistentVolume{}, "", "", false, fmt.Errorf("hold volume %s: %w", claim.Name, err)
	}

	row.InstanceID = pgtype.Text{String: instance.ID, Valid: true}

	return row, "", "", true, nil
}

func sortedClaims(claims []VolumeClaim) []VolumeClaim {
	sorted := slices.Clone(claims)
	slices.SortFunc(sorted, func(left, right VolumeClaim) int { return strings.Compare(left.Name, right.Name) })

	return sorted
}

// failForLimit fails a pending instance with the size limit's reason, and
// records instance.fail and limit_refused.
func (s PostgresStartStore) failForLimit(ctx context.Context, instance db.Instance, reason string) error {
	reason = truncate(reason, maxReasonBytes)

	changed, err := s.changeWith(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceFailed(ctx, db.SetInstanceFailedParams{ID: instance.ID, FromStatus: StatusPending, Reason: reason})
	}, func(_ pgx.Tx, queries *db.Queries) error {
		return audit.NewRecorder(s.auditOutput(), queries).Record(ctx, audit.Event{
			Type:   audit.TypeInstanceFail,
			Server: instance.TemplateName,
			Details: map[string]any{
				"instance": instance.ID,
				"reason":   reason,
				"owner":    instance.OwnerPrincipal,
			},
		})
	})
	if err != nil {
		return fmt.Errorf("fail instance %s: %w", instance.ID, err)
	}

	if changed {
		s.Metrics.ObserveVolumeEvent(VolumeEventLimitRefused)
		s.Metrics.observeStart(instance.TemplateName, StartFailed)
	}

	return nil
}

// LoseVolume implements VolumeStore.
func (s PostgresStartStore) LoseVolume(ctx context.Context, instance db.Instance, row db.PersistentVolume) (bool, error) {
	cleared := false

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		changed, err := queries.ClearPersistentVolumePV(ctx, db.ClearPersistentVolumePVParams{ID: row.ID, OldPvName: row.PvName})
		if err != nil || changed == 0 {
			return err
		}

		cleared = true

		return audit.NewRecorder(s.auditOutput(), queries).Record(ctx, audit.Event{
			Type:   audit.TypeVolumeLost,
			Server: row.TemplateName,
			Details: map[string]any{
				"volume":           row.VolumeName,
				"persistentVolume": row.PvName.String,
				"instance":         instance.ID,
			},
		})
	})
	if err != nil {
		return false, fmt.Errorf("lose volume %s: %w", row.ID, err)
	}

	if cleared {
		s.Metrics.ObserveVolumeEvent(VolumeEventLost)
	}

	return cleared, nil
}

// RecordVolume implements VolumeStore.
func (s PostgresStartStore) RecordVolume(ctx context.Context, instance db.Instance, row db.PersistentVolume, pvName string,
	sizeBytes int64,
) (bool, error) {
	recorded := false

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		changed, err := queries.RecordPersistentVolume(ctx, db.RecordPersistentVolumeParams{
			ID: row.ID, PvName: pgtype.Text{String: pvName, Valid: true}, SizeBytes: sizeBytes,
			InstanceID: pgtype.Text{String: instance.ID, Valid: true},
		})
		if err != nil || changed == 0 {
			return err
		}

		recorded = true

		return audit.NewRecorder(s.auditOutput(), queries).Record(ctx, audit.Event{
			Type:   audit.TypeVolumeCreate,
			Server: row.TemplateName,
			Details: map[string]any{
				"volume":           row.VolumeName,
				"scope":            row.Scope,
				"persistentVolume": pvName,
				"sizeBytes":        sizeBytes,
				"instance":         instance.ID,
			},
		})
	})
	if err != nil {
		return false, fmt.Errorf("record volume %s: %w", row.ID, err)
	}

	if recorded {
		s.Metrics.ObserveVolumeEvent(VolumeEventCreated)
	}

	return recorded, nil
}

// SetVolumeProblem implements VolumeStore.
func (s PostgresStartStore) SetVolumeProblem(ctx context.Context, id, problem string) error {
	err := db.New(s.Pool).SetPersistentVolumeProblem(ctx, db.SetPersistentVolumeProblemParams{
		ID: id, Problem: truncate(problem, store.MaxPersistentVolumeProblemBytes),
	})
	if err != nil {
		return fmt.Errorf("set problem of volume %s: %w", id, err)
	}

	return nil
}

// SetWaitReason implements VolumeStore. It notifies `instance:<id>` when the
// reason changed, so a waiting request sees it.
func (s PostgresStartStore) SetWaitReason(ctx context.Context, id, reason string) error {
	reason = truncate(reason, maxReasonBytes)

	_, err := s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		instance, err := queries.GetInstance(ctx, id)
		if err != nil || instance.Reason == reason {
			return 0, err
		}

		return queries.SetInstanceReason(ctx, db.SetInstanceReasonParams{ID: id, FromStatus: StatusPending, Reason: reason})
	})

	return err
}

// HeldVolumes implements VolumeStore.
func (s PostgresStartStore) HeldVolumes(ctx context.Context, instanceID string) ([]db.PersistentVolume, error) {
	rows, err := db.New(s.Pool).ListPersistentVolumesHeldBy(ctx, pgtype.Text{String: instanceID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list volumes of instance %s: %w", instanceID, err)
	}

	return rows, nil
}
