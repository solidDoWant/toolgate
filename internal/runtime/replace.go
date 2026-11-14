package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/telemetry"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// Reasons a live instance is replaced, written to its instance.stop event.
const (
	ReplaceReasonRestart        = "restart"
	ReplaceReasonResolverExpiry = "resolver-expiry"
	ReplaceReasonHealthCheck    = "health-check"
	ReplaceReasonTemplateUpdate = "template-update"
)

// Replacer replaces live instances with new pending rows at the template's
// current generation. Only the
// reconciler runs it.
type Replacer struct {
	Pool *pgxpool.Pool
	// Stops judges the stop conditions, which win over a replacement,
	// and gives the templates.
	Stops *StopDecider
	// AuditOutput receives the instance.stop events as JSON lines. Nil
	// discards them; the rows are still written.
	AuditOutput io.Writer
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	Log *slog.Logger
	// Metrics counts repairs. Nil means none.
	Metrics *Metrics
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

// ResolverExpiryMargin is how close to its resolved value's expiry a live
// instance is replaced.
const ResolverExpiryMargin = 60 * time.Second

// ExpiryStep replaces, with reason resolver-expiry, a live instance whose
// earliest resolved value expires in less than ResolverExpiryMargin. The
// replacement's render calls the resolvers again.
func (r *Replacer) ExpiryStep(ctx context.Context, instance db.Instance) error {
	if !isLive(instance) || !instance.ResolvedExpiresAt.Valid ||
		!instance.ResolvedExpiresAt.Time.Before(r.now().Add(testtime.Scale(ResolverExpiryMargin))) {
		return nil
	}

	_, err := r.Replace(ctx, instance, ReplaceReasonResolverExpiry, "")

	return err
}

// UpdateStep replaces, with reason template-update, a ready instance that
// was started from an older generation of its template when the template's
// update policy says so: at once with immediate, or with whenIdle once the
// instance has had no calls for idleFor. With manual, the default, it does
// nothing. The stop conditions still win, as in every replacement.
func (r *Replacer) UpdateStep(ctx context.Context, instance db.Instance) error {
	if !isLive(instance) || instance.Status != statusReady || !r.Stops.Templates.IsLoaded() {
		return nil
	}

	template, found := r.Stops.Templates.Running(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})
	if !found || string(template.UID) != instance.TemplateUid || template.Generation == instance.Generation {
		return nil
	}

	policy := template.Spec.UpdatePolicy
	if policy == nil {
		return nil
	}

	switch policy.Mode {
	case v1alpha1.UpdateModeImmediate:
	case v1alpha1.UpdateModeWhenIdle:
		if policy.IdleFor == nil {
			return nil
		}

		lastUsed := instance.LastUsedAt
		if !lastUsed.Valid {
			lastUsed = instance.ReadyAt
		}

		if !lastUsed.Valid || r.now().Sub(lastUsed.Time) < policy.IdleFor.Duration {
			return nil
		}
	default:
		return nil
	}

	_, err := r.Replace(ctx, instance, ReplaceReasonTemplateUpdate, "")

	return err
}

func (r *Replacer) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}

	return r.Now()
}

// Replace replaces one live instance in one transaction: it takes the
// advisory lock of the instance's template and scope key and re-reads the
// row. If the row isn't live any more, it does nothing. If one of the
// stop conditions holds, it stops the instance instead. Otherwise it sets
// desired = stopped, inserts a pending row with the same scope, owner and
// render session at the template's current generation, writes instance.stop
// with reason and user, and notifies both IDs. It skips the instance limits:
// a replacement doesn't change how many instances run. It returns the new
// row, or a zero row when nothing was replaced.
func (r *Replacer) Replace(ctx context.Context, instance db.Instance, reason, user string) (db.Instance, error) {
	return r.replace(ctx, instance, reason, user, false)
}

// replace is Replace. When repair is true, the replacement is a repair: its
// time is added to the repair history the replacement carries.
func (r *Replacer) replace(ctx context.Context, instance db.Instance, reason, user string, repair bool) (db.Instance, error) {
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

		cause, err := r.Stops.stopReason(ctx, queries, current)
		if err != nil {
			return err
		}

		if _, err := setDesiredStopped(ctx, queries, current.ID, cause); err != nil {
			return err
		}

		if cause.Code != "" {
			if err := recordInstanceStop(ctx, queries, r.AuditOutput, current, cause.Code, nil); err != nil {
				return err
			}

			r.Log.InfoContext(ctx, "stopping instance instead of replacing it", slog.String("instance", current.ID),
				slog.String("template", current.TemplateNs+"/"+current.TemplateName), slog.String("why", cause.Why))

			return store.Notify(ctx, tx, store.KindInstance, current.ID)
		}

		repairs := recentRepairs(current.Repairs, r.now())
		if repair {
			repairs = append(repairs, pgtype.Timestamptz{Time: r.now(), Valid: true})
		}

		replacement, err = r.insertReplacement(ctx, queries, current, repairs)
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

		if err := RecordInstanceStart(ctx, queries, output, replacement); err != nil {
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

	if repair && replacement.ID != "" {
		r.Metrics.observeRepair(instance.TemplateName, RepairReplace)
	}

	return replacement, nil
}

// insertReplacement inserts the pending row that replaces current. The stop
// conditions were checked, so the template exists with current's uid.
func (r *Replacer) insertReplacement(ctx context.Context, queries *db.Queries, current db.Instance,
	repairs []pgtype.Timestamptz,
) (db.Instance, error) {
	template, found := r.Stops.Templates.Running(types.NamespacedName{Namespace: current.TemplateNs, Name: current.TemplateName})
	if !found {
		return db.Instance{}, fmt.Errorf("template %s/%s is gone", current.TemplateNs, current.TemplateName)
	}

	exchanges := catalog.ExchangeNames(template)

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
		Repairs:        repairs,
		UsesExchange:   len(exchanges) > 0,
	})
	if err != nil {
		return db.Instance{}, fmt.Errorf("insert replacement: %w", err)
	}

	// The replacement waits for a token from its owner's next request: the
	// leader holds none, because it keeps no token after it applies it. With
	// persistent volumes the rows stay closed until the volume steps are done.
	closed := len(template.Spec.PersistentVolumes) > 0

	for _, exchange := range exchanges {
		err := queries.InsertInstanceExchange(ctx, db.InsertInstanceExchangeParams{
			InstanceID: id, Exchange: exchange, Closed: closed, Wanted: !closed, Claimed: false,
		})
		if err != nil {
			return db.Instance{}, fmt.Errorf("insert exchange %s of replacement: %w", exchange, err)
		}
	}

	return replacement, nil
}
