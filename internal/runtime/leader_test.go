package runtime_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
)

// memoryLock is a resource lock whose writes fail on demand.
type memoryLock struct {
	identity string
	fail     bool
}

func (m *memoryLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	return nil, nil, errors.New("not used")
}

func (m *memoryLock) Create(context.Context, resourcelock.LeaderElectionRecord) error {
	if m.fail {
		return errors.New("write failed")
	}

	return nil
}

func (m *memoryLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	return m.Create(ctx, record)
}

func (m *memoryLock) RecordEvent(string) {}
func (m *memoryLock) Identity() string   { return m.identity }
func (m *memoryLock) Describe() string   { return "memory/" + m.identity }

func TestLeaseHoldsLeaseOnlyWithinRenewDeadline(t *testing.T) {
	const renewDeadline = 10 * time.Second

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	inner := &memoryLock{identity: "replica-a"}
	lease := runtime.NewLeaseWithClock(inner, renewDeadline, func() time.Time { return now })

	assert.False(t, lease.HoldsLease(), "before any write")

	require.NoError(t, lease.Create(t.Context(), resourcelock.LeaderElectionRecord{HolderIdentity: "replica-a"}))
	assert.True(t, lease.HoldsLease(), "right after acquiring")

	now = start.Add(renewDeadline - time.Nanosecond)

	assert.True(t, lease.HoldsLease(), "just inside the renew deadline")

	now = start.Add(renewDeadline)

	assert.False(t, lease.HoldsLease(), "at the renew deadline without a renewal")

	require.NoError(t, lease.Update(t.Context(), resourcelock.LeaderElectionRecord{HolderIdentity: "replica-a"}))
	assert.True(t, lease.HoldsLease(), "after a renewal")

	inner.fail = true
	now = now.Add(renewDeadline)

	require.Error(t, lease.Update(t.Context(), resourcelock.LeaderElectionRecord{HolderIdentity: "replica-a"}))
	assert.False(t, lease.HoldsLease(), "a failed renewal does not extend the Lease")

	inner.fail = false

	require.NoError(t, lease.Update(t.Context(), resourcelock.LeaderElectionRecord{HolderIdentity: "replica-a"}))
	require.NoError(t, lease.Update(t.Context(), resourcelock.LeaderElectionRecord{HolderIdentity: ""}))
	assert.False(t, lease.HoldsLease(), "after releasing the Lease")

	require.NoError(t, lease.Update(t.Context(), resourcelock.LeaderElectionRecord{HolderIdentity: "replica-b"}))
	assert.False(t, lease.HoldsLease(), "when another replica is the holder")
}

// replica is one gateway replica's manager running the reconciler.
type replica struct {
	lease  *runtime.Lease
	passes atomic.Int32
	// stop cancels the manager and returns its result.
	stop func() error
}

func startReplica(t *testing.T, clientset *fake.Clientset, identity string) *replica {
	t.Helper()

	timing := runtime.ElectionTiming{
		LeaseDuration: 2 * time.Second,
		RenewDeadline: 1500 * time.Millisecond,
		RetryPeriod:   100 * time.Millisecond,
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: "toolgate", Name: runtime.LeaseName},
		Client:     clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}

	r := &replica{lease: runtime.NewLease(lock, timing.RenewDeadline)}

	r.stop = startManager(t, runtime.ManagerOptions(r.lease, timing), &runtime.Reconciler{
		Pass: func(context.Context) error {
			r.passes.Add(1)

			return nil
		},
		Subscribe: func() (<-chan store.Event, func()) { return make(chan store.Event), func() {} },
		Fence:     r.lease,
		Interval:  time.Hour,
	})

	return r
}

// startManager runs a manager with options and reconciler until the test
// ends, and returns a function that stops it and returns its result.
func startManager(t *testing.T, options manager.Options, reconciler *runtime.Reconciler) func() error {
	t.Helper()

	options.Metrics = metricsserver.Options{BindAddress: "0"}
	options.Controller.SkipNameValidation = ptr.To(true)

	// The manager never reaches this address: a Lease lives in a fake
	// clientset and the reconciler touches no other API.
	mgr, err := manager.New(&rest.Config{Host: "http://127.0.0.1:1"}, options)
	require.NoError(t, err)
	require.NoError(t, reconciler.SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	stop := sync.OnceValue(func() error {
		cancel()

		return <-done
	})

	t.Cleanup(func() { _ = stop() })

	return stop
}

func TestOnlyTheLeaseHolderRunsTheReconciler(t *testing.T) {
	clientset := fake.NewClientset()

	first := startReplica(t, clientset, "replica-a")

	require.Eventually(t, func() bool { return first.passes.Load() > 0 }, 10*time.Second, 10*time.Millisecond,
		"the first replica gains the Lease and runs a pass")
	assert.True(t, first.lease.HoldsLease())

	second := startReplica(t, clientset, "replica-b")

	// Wait until the second replica has tried to take the Lease and the
	// first has renewed it several times since.
	lease, err := clientset.CoordinationV1().Leases("toolgate").Get(t.Context(), runtime.LeaseName, metav1.GetOptions{})
	require.NoError(t, err)

	transitions := *lease.Spec.LeaseTransitions

	seen := lease.Spec.RenewTime.Time
	renewals := 0

	require.Eventually(t, func() bool {
		current, err := clientset.CoordinationV1().Leases("toolgate").Get(t.Context(), runtime.LeaseName, metav1.GetOptions{})
		if err == nil && current.Spec.RenewTime.After(seen) {
			seen = current.Spec.RenewTime.Time
			renewals++
		}

		return renewals >= 5
	}, 10*time.Second, 10*time.Millisecond, "the first replica keeps renewing the Lease")

	assert.Zero(t, second.passes.Load(), "the second replica does not run the reconciler while the first holds the Lease")
	assert.False(t, second.lease.HoldsLease())

	lease, err = clientset.CoordinationV1().Leases("toolgate").Get(t.Context(), runtime.LeaseName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "replica-a", *lease.Spec.HolderIdentity)
	assert.Equal(t, transitions, *lease.Spec.LeaseTransitions)

	// The first replica stops and releases the Lease; the second takes over.
	require.NoError(t, first.stop())

	lease, err = clientset.CoordinationV1().Leases("toolgate").Get(t.Context(), runtime.LeaseName, metav1.GetOptions{})
	require.NoError(t, err)

	if holder := *lease.Spec.HolderIdentity; holder != "replica-b" {
		assert.Empty(t, holder, "the stopping replica releases the Lease")
	}

	assert.False(t, first.lease.HoldsLease(), "a replica that released the Lease no longer holds it")

	require.Eventually(t, func() bool { return second.passes.Load() > 0 }, 10*time.Second, 10*time.Millisecond,
		"the second replica takes over the released Lease")
	assert.True(t, second.lease.HoldsLease())
}
