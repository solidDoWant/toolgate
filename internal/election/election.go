// Package election helps reconcilers that run on every replica but write
// only while their replica holds the leader Lease (docs/design/phase-1.md
// §1).
package election

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// IsElected reports whether elected is closed, which the manager does when
// this replica becomes leader (manager.Manager.Elected). A nil channel
// counts as elected, for a reconciler used without a manager.
func IsElected(elected <-chan struct{}) bool {
	if elected == nil {
		return true
	}

	select {
	case <-elected:
		return true
	default:
		return false
	}
}

// RequeueOnElection returns a source that, once this replica becomes leader,
// enqueues every object list returns, so the reconciler writes their status
// again. It adds a leader-only runnable to mgr. A failed list enqueues
// nothing; the objects' own events still reach the reconciler.
func RequeueOnElection(mgr manager.Manager, list func(ctx context.Context) ([]client.Object, error)) (source.Source, error) {
	events := make(chan event.GenericEvent)

	// A runnable needs leader election unless it says otherwise.
	err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		// The manager starts its leader-only runnables before it closes
		// Elected. An object enqueued before then is reconciled as on a
		// replica that isn't leader, and its leader-only write is lost.
		select {
		case <-mgr.Elected():
		case <-ctx.Done():
			return nil
		}

		objects, err := list(ctx)
		if err != nil {
			mgr.GetLogger().Error(err, "listing objects to re-queue after the election")

			return nil
		}

		for _, object := range objects {
			select {
			case events <- event.GenericEvent{Object: object}:
			case <-ctx.Done():
				return nil
			}
		}

		return nil
	}))
	if err != nil {
		return nil, err
	}

	return source.Channel(events, &handler.EnqueueRequestForObject{}), nil
}
