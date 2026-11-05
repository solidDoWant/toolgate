package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/store"
)

// DefaultPassInterval is how often the reconciler does a full pass over
// every row, besides the pass when it gains the Lease and those after
// notifications.
const DefaultPassInterval = 30 * time.Second

// ErrNotLeader is returned by Reconciler.Delete when this replica no longer
// holds the Lease.
var ErrNotLeader = errors.New("runtime: this replica no longer holds the leader Lease")

// Reconciler is the runtime reconciler loop. It is a controller-runtime
// runnable that needs leader election, so it runs only on the replica that
// holds the Lease, and stops when that replica loses it.
type Reconciler struct {
	// Pass does one full pass over every row.
	Pass func(ctx context.Context) error
	// Subscribe returns the PostgreSQL notifications and a function that
	// ends the subscription (store.Listener.Subscribe).
	Subscribe func() (<-chan store.Event, func())
	// Fence is checked before every Kubernetes delete.
	Fence Fence
	// Client deletes Kubernetes objects.
	Client client.Client
	// Interval between full passes. Zero means DefaultPassInterval.
	Interval time.Duration
	Log      *slog.Logger
}

// NeedLeaderElection makes the manager start the reconciler only on the
// Lease holder.
func (r *Reconciler) NeedLeaderElection() bool {
	return true
}

// Start runs a full pass at once, then every Interval and after every
// `instance:` or `session:` notification, until ctx ends. Notifications that
// arrive during a pass are folded into one following pass. A failed pass is
// logged; the next trigger retries.
func (r *Reconciler) Start(ctx context.Context) error {
	events, unsubscribe := r.Subscribe()
	defer unsubscribe()

	interval := r.Interval
	if interval == 0 {
		interval = DefaultPassInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.Log.InfoContext(ctx, "runtime reconciler started")

	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Log.ErrorContext(ctx, "runtime reconciler pass failed", slog.String("error", err.Error()))
		}

		if !r.wait(ctx, events, ticker.C) {
			r.Log.InfoContext(ctx, "runtime reconciler stopped")

			return nil
		}
	}
}

// wait blocks until the next pass is due and reports false when ctx ends.
func (r *Reconciler) wait(ctx context.Context, events <-chan store.Event, ticks <-chan time.Time) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticks:
			return true
		case event := <-events:
			if !triggersPass(event) {
				continue
			}

			drain(events)

			return true
		}
	}
}

func triggersPass(event store.Event) bool {
	return event.Missed || event.Kind == store.KindInstance || event.Kind == store.KindSession
}

// drain discards the events already buffered: the pass about to run covers
// them.
func drain(events <-chan store.Event) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

// Delete deletes object from Kubernetes, but only while this replica still
// holds the Lease. Otherwise it deletes nothing and returns ErrNotLeader.
func (r *Reconciler) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if !r.Fence.HoldsLease() {
		return ErrNotLeader
	}

	if err := r.Client.Delete(ctx, object, options...); err != nil {
		return fmt.Errorf("delete %s %s/%s: %w", object.GetObjectKind().GroupVersionKind().Kind,
			object.GetNamespace(), object.GetName(), err)
	}

	return nil
}
