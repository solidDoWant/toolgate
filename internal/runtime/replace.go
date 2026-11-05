package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/telemetry"
)

// Reasons a live instance is replaced, written to its instance.stop event.
const (
	ReplaceReasonRestart        = "restart"
	ReplaceReasonResolverExpiry = "resolver-expiry"
)

// Replacer replaces live instances with new pending rows at the template's
// current generation (docs/design/phase-2.md §6, "Restart"). Only the
// reconciler runs it.
type Replacer struct {
	Pool *pgxpool.Pool
	// Stops judges phase 1's stop conditions, which win over a replacement,
	// and gives the templates.
	Stops *StopDecider
	// AuditOutput receives the instance.stop events as JSON lines. Nil
	// discards them; the rows are still written.
	AuditOutput io.Writer
	Log         *slog.Logger
}

// Step replaces a live instance whose restart was requested. A new leader's
// first pass finds requests the old leader didn't finish.
func (r *Replacer) Step(ctx context.Context, instance db.Instance) error {
	if !isLive(instance) || !instance.RestartRequestedAt.Valid {
		return nil
	}

	_, err := r.Replace(ctx, instance, ReplaceReasonRestart, instance.RestartRequestedBy.String)

	return err
}

// Replace replaces one live instance in one transaction: it takes the
// advisory lock of the instance's template and scope key and re-reads the
// row. If the row isn't live any more, it does nothing. If one of phase 1's
// stop conditions holds, it stops the instance instead. Otherwise it sets
// desired = stopped, inserts a pending row with the same scope, owner and
// render session at the template's current generation, writes instance.stop
// with reason and user, and notifies both IDs. It skips the instance limits:
// a replacement doesn't change how many instances run. It returns the new
// row, or a zero row when nothing was replaced.
func (r *Replacer) Replace(ctx context.Context, instance db.Instance, reason, user string) (db.Instance, error) {
	if !r.Stops.Templates.IsLoaded() {
		return db.Instance{}, nil
	}

	var replacement db.Instance

	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		err := queries.LockInstanceScope(ctx, db.LockInstanceScopeParams{
			TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName, ScopeKey: instance.ScopeKey,
		})
		if err != nil {
			return err
		}

		current, err := queries.GetInstanceForUpdate(ctx, instance.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return err
		}

		if !isLive(current) {
			return nil
		}

		stopReason, err := r.Stops.stopReason(ctx, queries, current)
		if err != nil {
			return err
		}

		if _, err := queries.SetInstanceDesiredStopped(ctx, current.ID); err != nil {
			return err
		}

		if stopReason != "" {
			r.Log.InfoContext(ctx, "stopping instance instead of replacing it", slog.String("instance", current.ID),
				slog.String("template", current.TemplateNs+"/"+current.TemplateName), slog.String("why", stopReason))

			return store.Notify(ctx, tx, store.KindInstance, current.ID)
		}

		replacement, err = r.insertReplacement(ctx, queries, current)
		if err != nil {
			return err
		}

		output := r.AuditOutput
		if output == nil {
			output = io.Discard
		}

		err = audit.NewRecorder(output, queries).Record(ctx, audit.Event{
			Type:      audit.TypeInstanceStop,
			Principal: user,
			Server:    current.TemplateName,
			Details: map[string]any{
				"reason":      reason,
				"instance":    current.ID,
				"replacement": replacement.ID,
				"owner":       current.OwnerPrincipal,
			},
		})
		if err != nil {
			return err
		}

		if err := store.Notify(ctx, tx, store.KindInstance, current.ID); err != nil {
			return err
		}

		if err := store.Notify(ctx, tx, store.KindInstance, replacement.ID); err != nil {
			return err
		}

		r.Log.InfoContext(ctx, "replacing instance", slog.String("instance", current.ID),
			slog.String("replacement", replacement.ID), slog.String("why", reason))

		return nil
	})
	if err != nil {
		return db.Instance{}, fmt.Errorf("replace instance %s: %w", instance.ID, err)
	}

	return replacement, nil
}

// insertReplacement inserts the pending row that replaces current. The stop
// conditions were checked, so the template exists with current's uid.
func (r *Replacer) insertReplacement(ctx context.Context, queries *db.Queries, current db.Instance) (db.Instance, error) {
	template, found := r.Stops.Templates.Running(types.NamespacedName{Namespace: current.TemplateNs, Name: current.TemplateName})
	if !found {
		return db.Instance{}, fmt.Errorf("template %s/%s is gone", current.TemplateNs, current.TemplateName)
	}

	generated, err := uuid.NewV7()
	if err != nil {
		return db.Instance{}, fmt.Errorf("instance ID: %w", err)
	}

	id := hex.EncodeToString(generated[:])

	// An instance in the shared namespace is replaced in it; any other gets
	// a namespace of its own.
	namespace := NamespacePrefix + id
	if !hasOwnNamespace(current) {
		namespace = current.Namespace
	}

	lastUsed := current.LastUsedAt
	if !lastUsed.Valid {
		lastUsed = current.ReadyAt
	}

	replacement, err := queries.InsertReplacementInstance(ctx, db.InsertReplacementInstanceParams{
		ID:             id,
		TemplateNs:     current.TemplateNs,
		TemplateName:   current.TemplateName,
		TemplateUid:    string(template.UID),
		Generation:     template.Generation,
		Scope:          current.Scope,
		ScopeKey:       current.ScopeKey,
		OwnerPrincipal: current.OwnerPrincipal,
		RenderSession:  current.RenderSession,
		Namespace:      namespace,
		TraceContext:   telemetry.EncodeContext(ctx),
		LastUsedAt:     pgtype.Timestamptz{Time: lastUsed.Time, Valid: lastUsed.Valid},
	})
	if err != nil {
		return db.Instance{}, fmt.Errorf("insert replacement: %w", err)
	}

	return replacement, nil
}
