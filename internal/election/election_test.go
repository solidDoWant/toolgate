package election_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/internal/election"
)

// fakeManager records the runnables added to it and reports elected as its
// Elected channel. Other manager methods are not used.
type fakeManager struct {
	manager.Manager

	elected   chan struct{}
	runnables []manager.Runnable
}

func (m *fakeManager) Add(runnable manager.Runnable) error {
	m.runnables = append(m.runnables, runnable)

	return nil
}

func (m *fakeManager) Elected() <-chan struct{} { return m.elected }

func (m *fakeManager) GetLogger() logr.Logger { return logr.Discard() }

func TestRequeueOnElection(t *testing.T) {
	// The manager starts its leader-only runnables before it closes
	// Elected, so the re-queue runnable runs while Elected is still open. A
	// reconciler that saw the object then would skip its leader-only write.
	t.Run("objects are enqueued only once Elected is closed", func(t *testing.T) {
		mgr := &fakeManager{elected: make(chan struct{})}

		var listedBeforeElected, listed atomic.Bool

		list := func(context.Context) ([]client.Object, error) {
			listed.Store(true)

			if !election.IsElected(mgr.elected) {
				listedBeforeElected.Store(true)
			}

			return []client.Object{
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "watched", Name: "first"}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "watched", Name: "second"}},
			}, nil
		}

		requeue, err := election.RequeueOnElection(mgr, list)
		require.NoError(t, err)
		require.Len(t, mgr.runnables, 1)

		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
		t.Cleanup(queue.ShutDown)

		require.NoError(t, requeue.Start(t.Context(), queue))

		done := make(chan error, 1)

		go func() { done <- mgr.runnables[0].Start(t.Context()) }()

		assert.Never(t, listed.Load, 200*time.Millisecond, 10*time.Millisecond,
			"nothing is listed before this replica is elected")
		assert.Zero(t, queue.Len(), "nothing is enqueued before this replica is elected")

		close(mgr.elected)

		require.Eventually(t, func() bool { return queue.Len() == 2 }, 10*time.Second, 10*time.Millisecond,
			"every listed object is enqueued after the election")
		assert.False(t, listedBeforeElected.Load(), "the objects are listed after the election")

		var keys []types.NamespacedName

		for range 2 {
			request, shutdown := queue.Get()
			require.False(t, shutdown)

			keys = append(keys, request.NamespacedName)
			queue.Done(request)
		}

		assert.ElementsMatch(t, []types.NamespacedName{
			{Namespace: "watched", Name: "first"},
			{Namespace: "watched", Name: "second"},
		}, keys)

		require.NoError(t, <-done)
	})
}
