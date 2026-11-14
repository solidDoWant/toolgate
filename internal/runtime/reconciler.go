package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// DefaultPassInterval is how often the reconciler does a full pass over
// every row, besides the pass when it gains the Lease and those after
// notifications.
const DefaultPassInterval = 30 * time.Second

// ErrNotLeader is returned by Reconciler.Delete when this replica no longer
// holds the Lease.
var ErrNotLeader = errors.New("runtime: this replica no longer holds the leader Lease")

// Reconciler is the runtime reconciler. It is a controller-runtime
// controller that needs leader election, so it runs only on the replica that
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
	// TemplateSecrets is the cache of the template secrets namespace. Each
	// event on one of its Secrets triggers a pass. Nil when the feature is
	// off.
	TemplateSecrets cache.Cache
}

// passRequest is the work queue item for a full pass. There is only one, so
// triggers that arrive while a pass waits or runs fold into one more pass.
type passRequest struct{}

// SetupWithManager registers the reconciler with mgr. On the Lease holder it
// runs a full pass at once, then every Interval and after every `instance:`
// or `session:` notification, or after notifications were missed. A failed
// pass is retried with backoff.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	notifications := make(chan event.TypedGenericEvent[store.Event])

	// A runnable needs leader election unless it says otherwise.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return r.feed(ctx, notifications)
	})); err != nil {
		return fmt.Errorf("runtime reconciler: %w", err)
	}

	controller := builder.TypedControllerManagedBy[passRequest](mgr).Named("runtime")

	if r.TemplateSecrets != nil {
		controller = controller.WatchesRawSource(source.TypedKind(r.TemplateSecrets, &corev1.Secret{},
			handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *corev1.Secret) []passRequest {
				return []passRequest{{}}
			})))
	}

	return controller.
		WatchesRawSource(source.TypedChannel(notifications, handler.TypedFuncs[store.Event, passRequest]{
			GenericFunc: func(_ context.Context, notification event.TypedGenericEvent[store.Event], queue workqueue.TypedRateLimitingInterface[passRequest]) {
				if triggersPass(notification.Object) {
					queue.Add(passRequest{})
				}
			},
		})).
		Complete(r)
}

// feed subscribes to the notifications and passes them to the controller,
// starting with a Missed event: on gaining the Lease, everything counts as
// changed.
func (r *Reconciler) feed(ctx context.Context, notifications chan<- event.TypedGenericEvent[store.Event]) error {
	events, unsubscribe := r.Subscribe()
	defer unsubscribe()

	next := store.Event{Missed: true}

	for {
		select {
		case <-ctx.Done():
			return nil
		case notifications <- event.TypedGenericEvent[store.Event]{Object: next}:
		}

		select {
		case <-ctx.Done():
			return nil
		case next = <-events:
		}
	}
}

func triggersPass(event store.Event) bool {
	return event.Missed || event.Kind == store.KindInstance || event.Kind == store.KindSession
}

// Reconcile runs one full pass and schedules the next one.
func (r *Reconciler) Reconcile(ctx context.Context, _ passRequest) (reconcile.Result, error) {
	if err := r.Pass(ctx); err != nil {
		return reconcile.Result{}, fmt.Errorf("runtime reconciler pass: %w", err)
	}

	interval := r.Interval
	if interval == 0 {
		interval = testtime.Scale(DefaultPassInterval)
	}

	return reconcile.Result{RequeueAfter: interval}, nil
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
