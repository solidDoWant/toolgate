package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const (
	// StuckAfter is how long a namespace may stay Terminating before the
	// reconciler warns and records the namespace's conditions as the reason.
	StuckAfter = 10 * time.Minute
	// StoppedRetention is how long stopped rows are kept for the audit
	// trail.
	StoppedRetention = 7 * 24 * time.Hour
)

// Deleter deletes Kubernetes objects. Reconciler.Delete implements it, and
// deletes only while this replica holds the Lease.
type Deleter interface {
	Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error
}

// Teardown moves failed and stopped instances through stopping to stopped,
// deleting their namespaces (docs/design/phase-1.md §2).
type Teardown struct {
	Pool *pgxpool.Pool
	// Reader reads instance namespaces.
	Reader  client.Reader
	Deleter Deleter
	Log     *slog.Logger
	Metrics *Metrics
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time

	// stuck counts the stuck namespaces seen in the current pass.
	stuck atomic.Int64
}

// Metrics are the instance metrics the reconciler keeps.
type Metrics struct {
	// Instances counts instance rows that aren't stopped, by scope and
	// status.
	Instances *prometheus.GaugeVec
	// NamespacesStuck counts instance namespaces Terminating for longer
	// than StuckAfter.
	NamespacesStuck prometheus.Gauge
}

// NewMetrics returns unregistered instance metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		Instances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toolgate_instances",
			Help: "Instances that aren't stopped, by scope and status. Set by the replica holding the leader Lease.",
		}, []string{"scope", "status"}),
		NamespacesStuck: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "toolgate_instance_namespaces_stuck",
			Help: "Instance namespaces Terminating for more than 10 minutes. Set by the replica holding the leader Lease.",
		}),
	}
}

// Collectors returns the metrics for registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.Instances, m.NamespacesStuck}
}

// Step takes one instance one step towards stopped. It is a Step of the
// full pass, which must be wrapped with Pass.
func (t *Teardown) Step(ctx context.Context, instance db.Instance) error {
	stuck, err := t.Advance(ctx, instance)
	if stuck {
		t.stuck.Add(1)
	}

	return err
}

// Pass wraps a full pass that runs Step. After the pass it deletes stopped
// rows older than StoppedRetention and sets the metrics.
func (t *Teardown) Pass(queries *db.Queries, pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		t.stuck.Store(0)

		err := pass(ctx)

		return errors.Join(err, t.DeleteExpired(ctx), t.Metrics.update(ctx, queries, int(t.stuck.Load())))
	}
}

// Advance takes one instance one step towards stopped. It returns true when
// the instance's namespace has been Terminating for longer than StuckAfter.
func (t *Teardown) Advance(ctx context.Context, instance db.Instance) (bool, error) {
	if instance.Status == statusFailed || (instance.Desired == desiredStopped && isActive(instance.Status)) {
		changed, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
			return queries.SetInstanceStopping(ctx, db.SetInstanceStoppingParams{ID: instance.ID, FromStatus: instance.Status})
		})
		if err != nil || !changed {
			// The row changed under us; the next pass reads it again.
			return false, err
		}

		instance.Status = statusStopping
	}

	if instance.Status != statusStopping {
		return false, nil
	}

	var namespace corev1.Namespace

	err := t.Reader.Get(ctx, types.NamespacedName{Name: instance.Namespace}, &namespace)
	if apierrors.IsNotFound(err) {
		_, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
			return queries.SetInstanceStopped(ctx, instance.ID)
		})

		return false, err
	}

	if err != nil {
		return false, fmt.Errorf("read namespace of instance %s: %w", instance.ID, err)
	}

	// Only a namespace made for this instance is deleted, never a shared
	// one.
	if namespace.Labels[LabelInstance] != instance.ID {
		reason := fmt.Sprintf("Namespace %s doesn't carry the label %s=%s, so it isn't deleted.", namespace.Name, LabelInstance, instance.ID)

		return false, t.setReason(ctx, instance, reason)
	}

	if namespace.DeletionTimestamp == nil {
		if err := t.Deleter.Delete(ctx, &namespace, client.Preconditions{UID: &namespace.UID}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}

		return false, nil
	}

	if t.now().Sub(namespace.DeletionTimestamp.Time) <= StuckAfter {
		return false, nil
	}

	reason := truncate("Namespace "+namespace.Name+" is still terminating: "+namespaceConditions(&namespace), maxReasonBytes)
	if reason != instance.Reason {
		t.Log.WarnContext(ctx, "instance namespace is still terminating", slog.String("instance", instance.ID),
			slog.String("namespace", namespace.Name), slog.Time("deletionTimestamp", namespace.DeletionTimestamp.Time))
	}

	return true, t.setReason(ctx, instance, reason)
}

// DeleteExpired deletes stopped rows older than StoppedRetention.
func (t *Teardown) DeleteExpired(ctx context.Context) error {
	if _, err := db.New(t.Pool).DeleteStoppedInstancesBefore(ctx, pgtype.Timestamptz{Time: t.now().Add(-StoppedRetention), Valid: true}); err != nil {
		return fmt.Errorf("delete old stopped instances: %w", err)
	}

	return nil
}

// setReason records reason on a stopping row, unless it is already there.
func (t *Teardown) setReason(ctx context.Context, instance db.Instance, reason string) error {
	if instance.Reason == reason {
		return nil
	}

	_, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceReason(ctx, db.SetInstanceReasonParams{ID: instance.ID, Reason: reason, FromStatus: statusStopping})
	})

	return err
}

// write runs one compare-and-set update in a transaction and, if it changed
// the row, notifies instance:<id> in the same transaction.
func (t *Teardown) write(ctx context.Context, id string, update func(queries *db.Queries) (int64, error)) (bool, error) {
	tx, err := t.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("update instance %s: %w", id, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	changed, err := update(db.New(tx))
	if err != nil {
		return false, fmt.Errorf("update instance %s: %w", id, err)
	}

	if changed == 0 {
		return false, nil
	}

	if err := store.Notify(ctx, tx, store.KindInstance, id); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("update instance %s: %w", id, err)
	}

	return true, nil
}

func (t *Teardown) now() time.Time {
	if t.Now == nil {
		return time.Now()
	}

	return t.Now()
}

// isActive reports whether a status is one a stop moves to stopping.
func isActive(status string) bool {
	return status == StatusPending || status == StatusStarting || status == statusReady
}

// namespaceConditions describes the namespace's conditions that are true,
// which say what blocks its deletion.
func namespaceConditions(namespace *corev1.Namespace) string {
	var parts []string

	for _, condition := range namespace.Status.Conditions {
		if condition.Status == corev1.ConditionTrue {
			parts = append(parts, string(condition.Type)+": "+condition.Message)
		}
	}

	if len(parts) == 0 {
		return "no conditions reported"
	}

	return strings.Join(parts, "; ")
}

// update sets the metrics from the rows that aren't stopped and the number
// of stuck namespaces. A nil Metrics does nothing.
func (m *Metrics) update(ctx context.Context, queries *db.Queries, stuck int) error {
	if m == nil {
		return nil
	}

	instances, err := queries.ListInstancesNotStopped(ctx)
	if err != nil {
		return fmt.Errorf("count instances: %w", err)
	}

	m.Instances.Reset()

	for _, instance := range instances {
		m.Instances.WithLabelValues(instance.Scope, instance.Status).Inc()
	}

	m.NamespacesStuck.Set(float64(stuck))

	return nil
}
