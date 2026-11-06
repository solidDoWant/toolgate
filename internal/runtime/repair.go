package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// How an unhealthy instance is repaired (docs/design/phase-3.md §6).
const (
	OnFailureReplace = "replace"
	OnFailureReapply = "reapply"
	OnFailureNone    = "none"
	// RepairWindow is the time in which at most maxRepairs repairs are
	// made.
	RepairWindow      = 30 * time.Minute
	defaultMaxRepairs = 3
)

// Repairer repairs ready instances that are unhealthy, as their health
// check's onFailure says, and fails an instance whose repairs are used up.
// Only the reconciler runs it, so a new leader's first pass makes a repair
// the old leader didn't make.
type Repairer struct {
	Pool *pgxpool.Pool
	// Replacer makes the replace repairs.
	Replacer *Replacer
	// Starter renders and applies the objects of a reapply repair.
	Starter *Starter
	// AuditOutput receives the instance.reapply and instance.fail events.
	// Nil discards them; the rows are still written.
	AuditOutput io.Writer
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	Log *slog.Logger
}

func (r *Repairer) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}

	return r.Now()
}

// repairPolicy is what an instance's stored health check says about
// repairs, with defaults filled in. An instance without a custom check is
// replaced, with the default bound.
type repairPolicy struct {
	onFailure  string
	maxRepairs int
	// recheck is how long a reapply waits before it counts the instance as
	// still unhealthy: failureThreshold × interval.
	recheck time.Duration
}

func repairPolicyOf(instance db.Instance) (repairPolicy, error) {
	policy := repairPolicy{
		onFailure:  OnFailureReplace,
		maxRepairs: defaultMaxRepairs,
		recheck:    defaultCheckFailureThreshold * testtime.Scale(defaultCheckInterval),
	}

	check, found, err := decodeHealthCheck(instance.HealthCheck)
	if err != nil || !found {
		return policy, err
	}

	if check.OnFailure != "" {
		policy.onFailure = check.OnFailure
	}

	if check.MaxRepairs > 0 {
		policy.maxRepairs = int(check.MaxRepairs)
	}

	policy.recheck = time.Duration(check.FailureThreshold) * check.Interval.Duration

	return policy, nil
}

// recentRepairs returns the repair times within RepairWindow before now.
func recentRepairs(repairs []pgtype.Timestamptz, now time.Time) []pgtype.Timestamptz {
	recent := make([]pgtype.Timestamptz, 0, len(repairs)+1)

	for _, repair := range repairs {
		if repair.Valid && now.Sub(repair.Time) < RepairWindow {
			recent = append(recent, repair)
		}
	}

	return recent
}

// Step repairs one instance, if it is ready, unhealthy and a repair is due.
func (r *Repairer) Step(ctx context.Context, instance db.Instance) error {
	if instance.Desired != "running" || instance.Status != StatusReady || !instance.UnhealthySince.Valid {
		return nil
	}

	policy, err := repairPolicyOf(instance)
	if err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	if policy.onFailure == OnFailureNone {
		return nil
	}

	now := r.now()

	var last time.Time

	for _, repair := range instance.Repairs {
		if repair.Valid && repair.Time.After(last) {
			last = repair.Time
		}
	}

	// A repair made since the instance became unhealthy is the one for
	// this time; a reapply counts again when the instance is still
	// unhealthy a whole detection later.
	if !last.Before(instance.UnhealthySince.Time) {
		if policy.onFailure != OnFailureReapply || now.Sub(last) < policy.recheck {
			return nil
		}
	}

	if recent := recentRepairs(instance.Repairs, now); len(recent) >= policy.maxRepairs {
		return r.giveUp(ctx, instance, len(recent))
	}

	switch policy.onFailure {
	case OnFailureReapply:
		return r.reapply(ctx, instance, now)
	default:
		_, err := r.Replacer.replace(ctx, instance, ReplaceReasonHealthCheck, "", true)

		return err
	}
}

// GiveUpReason is the reason an instance fails when its repairs are used
// up.
func GiveUpReason(repairs int, lastReason string) string {
	return fmt.Sprintf("Health check still failing after %d repairs in 30 minutes: %s", repairs, lastReason)
}

// giveUp fails the ready row, as phase 1 fails an instance, and writes
// instance.fail. The teardown removes its objects.
func (r *Repairer) giveUp(ctx context.Context, instance db.Instance, repairs int) error {
	reason := truncate(GiveUpReason(repairs, instance.UnhealthyReason), maxReasonBytes)

	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		changed, err := queries.SetInstanceFailed(ctx, db.SetInstanceFailedParams{
			ID: instance.ID, FromStatus: StatusReady, Reason: reason,
		})
		if err != nil || changed == 0 {
			return err
		}

		err = audit.NewRecorder(r.output(), queries).Record(ctx, audit.Event{
			Type:   audit.TypeInstanceFail,
			Server: instance.TemplateName,
			Details: map[string]any{
				"instance": instance.ID,
				"reason":   reason,
				"owner":    instance.OwnerPrincipal,
			},
		})
		if err != nil {
			return err
		}

		r.Log.InfoContext(ctx, "instance repairs used up; failing it", slog.String("instance", instance.ID),
			slog.Int("repairs", repairs))

		return store.Notify(ctx, tx, store.KindInstance, instance.ID)
	})
	if err != nil {
		return fmt.Errorf("fail instance %s: %w", instance.ID, err)
	}

	return nil
}

// reapply records the repair and writes instance.reapply, then renders the
// instance again and server-side applies its objects. An instance of another
// template generation or uid is replaced instead. The repair is
// recorded first, so an apply that keeps failing still uses up the bound.
func (r *Repairer) reapply(ctx context.Context, instance db.Instance, now time.Time) error {
	template, found := r.Starter.Template(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})
	if !found {
		// The stop path handles a deleted template.
		return nil
	}

	if template.Generation != instance.Generation || string(template.UID) != instance.TemplateUid {
		// The catalog holds only the template's current generation, so
		// the instance can't be rendered at its own: it is replaced
		// instead, and that counts as the repair.
		r.Log.InfoContext(ctx, "replacing an unhealthy instance of another template generation instead of reapplying",
			slog.String("instance", instance.ID), slog.Int64("generation", instance.Generation),
			slog.Int64("template generation", template.Generation))

		_, err := r.Replacer.replace(ctx, instance, ReplaceReasonHealthCheck, "", true)

		return err
	}

	recorded := false

	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		changed, err := queries.AddInstanceRepair(ctx, db.AddInstanceRepairParams{
			ID: instance.ID, RepairedAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil || changed == 0 {
			return err
		}

		recorded = true

		return audit.NewRecorder(r.output(), queries).Record(ctx, audit.Event{
			Type:    audit.TypeInstanceReapply,
			Server:  instance.TemplateName,
			Details: map[string]any{"instance": instance.ID, "owner": instance.OwnerPrincipal},
		})
	})
	if err != nil {
		return fmt.Errorf("reapply instance %s: %w", instance.ID, err)
	}

	if !recorded {
		return nil
	}

	r.Log.InfoContext(ctx, "reapplying unhealthy instance", slog.String("instance", instance.ID))

	identity, err := r.Starter.identity(ctx, instance)
	if err != nil {
		return err
	}

	result, err := Render(ctx, Input{
		Template:  template,
		Identity:  identity,
		Instance:  Instance{ID: instance.ID, Namespace: instance.Namespace},
		Secrets:   r.Starter.Secrets(identity.PrincipalID),
		Resolvers: r.Starter.Resolvers,
	})
	if err != nil {
		return fmt.Errorf("reapply instance %s: render: %w", instance.ID, err)
	}

	spanAttributes := trace.WithAttributes(attribute.String("toolgate.server", template.Name),
		attribute.String("toolgate.instance", instance.ID))

	if _, err := r.Starter.applyAll(ctx, instance, template, result, spanAttributes); err != nil {
		return fmt.Errorf("reapply instance %s: %w", instance.ID, err)
	}

	return nil
}

func (r *Repairer) output() io.Writer {
	if r.AuditOutput == nil {
		return io.Discard
	}

	return r.AuditOutput
}
