package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
)

// fakeFence is a fence whose answer the test sets.
type fakeFence struct{ holds atomic.Bool }

func (f *fakeFence) HoldsLease() bool { return f.holds.Load() }

// startLoop runs a reconciler without leader election, whose passes are
// reported on the returned channel, and returns the channel that feeds it
// notifications.
func startLoop(t *testing.T, interval time.Duration) (chan<- store.Event, <-chan struct{}) {
	t.Helper()

	events := make(chan store.Event, 16)
	passes := make(chan struct{}, 64)

	startManager(t, manager.Options{}, &runtime.Reconciler{
		Pass: func(context.Context) error {
			passes <- struct{}{}

			return nil
		},
		Subscribe: func() (<-chan store.Event, func()) { return events, func() {} },
		Fence:     &fakeFence{},
		Interval:  interval,
	})

	return events, passes
}

func requirePass(t *testing.T, passes <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-passes:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no pass", message)
	}
}

func TestReconcilerPassTriggers(t *testing.T) {
	assert.Equal(t, 30*time.Second, runtime.DefaultPassInterval)

	t.Run("a full pass runs when the Lease is gained", func(t *testing.T) {
		_, passes := startLoop(t, time.Hour)

		requirePass(t, passes, "the first pass runs at start")
	})

	t.Run("a full pass runs every interval", func(t *testing.T) {
		_, passes := startLoop(t, 20*time.Millisecond)

		for range 3 {
			requirePass(t, passes, "passes repeat on the interval")
		}
	})

	tests := []struct {
		name  string
		event store.Event
	}{
		{name: "instance notification", event: store.Event{Kind: store.KindInstance, ID: "0190abcd"}},
		{name: "session notification", event: store.Event{Kind: store.KindSession, ID: "42"}},
		{name: "missed notifications", event: store.Event{Missed: true}},
	}

	for _, test := range tests {
		t.Run("a full pass runs on an "+test.name, func(t *testing.T) {
			events, passes := startLoop(t, time.Hour)

			requirePass(t, passes, "the first pass runs at start")

			events <- test.event

			requirePass(t, passes, "the notification runs a pass")
		})
	}
}

func TestReconcilerDeletesOnlyWhileHoldingTheLease(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tg-0123"}}
	kube := clientfake.NewClientBuilder().WithObjects(namespace.DeepCopy()).Build()
	fence := &fakeFence{}
	reconciler := &runtime.Reconciler{Fence: fence, Client: kube}

	fence.holds.Store(false)

	err := reconciler.Delete(t.Context(), namespace.DeepCopy())
	require.ErrorIs(t, err, runtime.ErrNotLeader)
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(namespace), &corev1.Namespace{}),
		"a replica that lost the Lease deletes nothing")

	fence.holds.Store(true)

	require.NoError(t, reconciler.Delete(t.Context(), namespace.DeepCopy()))
	err = kube.Get(t.Context(), client.ObjectKeyFromObject(namespace), &corev1.Namespace{})
	require.True(t, apierrors.IsNotFound(err), "the leader deletes: %v", err)
}
