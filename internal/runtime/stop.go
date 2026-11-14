package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Instance scopes and the values of instances.desired and instances.status.
const (
	scopePerSession = "per-session"
	scopePerUser    = "per-user"

	desiredRunning = "running"
	desiredStopped = "stopped"

	statusReady    = "ready"
	statusFailed   = "failed"
	statusStopping = "stopping"
	statusStopped  = "stopped"
)

// Templates gives the template running instances are judged against: the
// last valid generation, which stays while a template is invalid and is gone
// once it is deleted. catalog.Catalog implements it.
type Templates interface {
	Running(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool)
	// IsLoaded reports whether every watched template has been read. Before
	// that, a missing template may only be not read yet.
	IsLoaded() bool
}

// StopDecider decides which live instances stop. Only the reconciler runs it.
type StopDecider struct {
	Pool *pgxpool.Pool
	// Templates must hold every watched template before the first pass.
	Templates Templates
	Log       *slog.Logger
	// Now returns the current time, used for idle stops. Nil means
	// time.Now.
	Now func() time.Time
}

// Step is Decide as a Step of the full pass. A stop it decides is torn
// down on the next pass, which its notification starts.
func (d *StopDecider) Step(ctx context.Context, instance db.Instance) error {
	_, err := d.Decide(ctx, instance)

	return err
}

// Decide checks one instance row. When a stop condition holds, it takes the
// advisory lock of the instance's template and scope key in a transaction,
// re-reads the row, checks again, and sets desired = stopped. It returns the
// row as it is afterwards.
//
// Until Templates is loaded it decides nothing: judging against a partial
// catalog would stop instances whose template exists.
func (d *StopDecider) Decide(ctx context.Context, instance db.Instance) (db.Instance, error) {
	if !isLive(instance) || !d.Templates.IsLoaded() {
		return instance, nil
	}

	reason, err := d.stopReason(ctx, db.New(d.Pool), instance)
	if err != nil || reason == "" {
		return instance, err
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return instance, fmt.Errorf("stop instance %s: %w", instance.ID, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)

	err = queries.LockInstanceScope(ctx, db.LockInstanceScopeParams{
		TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName, ScopeKey: instance.ScopeKey,
	})
	if err != nil {
		return instance, fmt.Errorf("stop instance %s: %w", instance.ID, err)
	}

	current, err := queries.GetInstance(ctx, instance.ID)
	if err != nil {
		return instance, fmt.Errorf("stop instance %s: %w", instance.ID, err)
	}

	if !isLive(current) {
		return current, nil
	}

	reason, err = d.stopReason(ctx, queries, current)
	if err != nil || reason == "" {
		return current, err
	}

	changed, err := setDesiredStopped(ctx, queries, current.ID, reason)
	if err != nil {
		return current, fmt.Errorf("stop instance %s: %w", current.ID, err)
	}

	if changed == 1 {
		if err := store.Notify(ctx, tx, store.KindInstance, current.ID); err != nil {
			return current, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return current, fmt.Errorf("stop instance %s: %w", current.ID, err)
	}

	d.Log.InfoContext(ctx, "stopping instance", slog.String("instance", current.ID),
		slog.String("template", current.TemplateNs+"/"+current.TemplateName), slog.String("why", reason))

	current.Desired = desiredStopped

	return current, nil
}

// exchangeExpiredReasonPrefix starts the stop reason of an instance whose
// exchanged token expired. Its stop write also marks the row.
const exchangeExpiredReasonPrefix = "its exchanged token "

// setDesiredStopped writes desired = stopped for the stop reason, marking
// the row credential_expired when an exchanged token expired.
func setDesiredStopped(ctx context.Context, queries *db.Queries, id, reason string) (int64, error) {
	if strings.HasPrefix(reason, exchangeExpiredReasonPrefix) {
		return queries.SetInstanceDesiredStoppedExpired(ctx, id)
	}

	return queries.SetInstanceDesiredStopped(ctx, id)
}

// stopReason returns why the instance must stop, or "" if it keeps running.
func (d *StopDecider) stopReason(ctx context.Context, queries *db.Queries, instance db.Instance) (string, error) {
	template, found := d.Templates.Running(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})

	switch {
	case !found:
		return "the template was deleted", nil
	case string(template.UID) != instance.TemplateUid:
		return "the template was deleted and created again", nil
	case template.Spec.Scope != instance.Scope:
		return "the template's scope changed", nil
	}

	if instance.OwnerPrincipal != "" {
		allowed, err := ownerHasAccess(ctx, queries, instance.OwnerPrincipal, template.Spec.Access)
		if err != nil {
			return "", err
		}

		if !allowed {
			return "the owner no longer has access to the template", nil
		}
	}

	expired, err := queries.FirstExpiredInstanceExchange(ctx, db.FirstExpiredInstanceExchangeParams{
		InstanceID: instance.ID, Now: pgtype.Timestamptz{Time: d.now(), Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read exchanged tokens of instance %s: %w", instance.ID, err)
	}

	if err == nil {
		return exchangeExpiredReasonPrefix + "`" + expired + "` expired", nil
	}

	enablers, err := queries.CountLiveEnablers(ctx, db.CountLiveEnablersParams{
		TemplateNs: instance.TemplateNs, TemplateName: instance.TemplateName,
		Scope: instance.Scope, OwnerPrincipal: instance.OwnerPrincipal, ScopeKey: instance.ScopeKey,
	})
	if err != nil {
		return "", fmt.Errorf("count sessions enabling instance %s: %w", instance.ID, err)
	}

	if enablers > 0 {
		return "", nil
	}

	if instance.Scope == scopePerSession {
		return "its session no longer has it enabled", nil
	}

	var idleStop time.Duration
	if template.Spec.IdleStop != nil {
		idleStop = template.Spec.IdleStop.Duration
	}

	if instance.Scope == scopeShared && idleStop == 0 {
		return "", nil
	}

	lastUsed := instance.LastUsedAt
	if !lastUsed.Valid {
		lastUsed = instance.ReadyAt
	}

	if lastUsed.Valid && d.now().Sub(lastUsed.Time) > idleStop {
		return "no session has it enabled and it is idle", nil
	}

	return "", nil
}

// ownerHasAccess judges access from the principal's most recently seen
// session snapshot. A principal with no session keeps access: there is
// nothing to judge from.
func ownerHasAccess(ctx context.Context, queries *db.Queries, principal string, access v1alpha1.Access) (bool, error) {
	session, err := queries.LatestSessionOfPrincipal(ctx, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("read the latest session of an instance owner: %w", err)
	}

	return policy.HasAccess(snapshotIdentity(session), access), nil
}

// snapshotIdentity is the identity a session snapshot was taken from. Its
// kind is the one recorded on the session, never inferred from the
// attestation or the principal's form.
func snapshotIdentity(session db.Session) auth.Identity {
	return auth.Identity{
		Principal:   session.Principal,
		Email:       session.Email,
		Groups:      session.Groups,
		Workspace:   session.Workspace,
		Session:     session.SessionID,
		Kind:        session.Kind,
		Attestation: session.Attestation,
	}
}

func (d *StopDecider) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}

	return d.Now()
}

// isLive reports whether the row is a live instance: desired running and not
// stopping or stopped.
func isLive(instance db.Instance) bool {
	return instance.Desired == desiredRunning && instance.Status != statusStopping && instance.Status != statusStopped
}
