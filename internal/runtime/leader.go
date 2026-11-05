package runtime

import (
	"context"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// LeaseName is the name of the Lease that elects the replica running the
// runtime reconciler.
const LeaseName = "toolgate-runtime"

// ElectionTiming holds the leader-election durations (client-go's
// leaderelection semantics).
type ElectionTiming struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// DefaultElectionTiming is the timing the gateway uses.
var DefaultElectionTiming = ElectionTiming{
	LeaseDuration: 15 * time.Second,
	RenewDeadline: 10 * time.Second,
	RetryPeriod:   2 * time.Second,
}

// Fence reports whether this replica still holds the Lease. The reconciler
// asks it before every Kubernetes delete.
type Fence interface {
	HoldsLease() bool
}

// Lease wraps a client-go resource lock and records each successful write
// that leaves this replica as the holder. It holds the Lease while the last
// such write started less than the renew deadline ago. Other replicas take
// the Lease only after the lease duration, which is longer, so a replica
// for which HoldsLease is true is still the only leader.
type Lease struct {
	resourcelock.Interface

	renewDeadline time.Duration
	now           func() time.Time

	mu        sync.Mutex
	held      bool
	renewedAt time.Time
}

// NewLease wraps lock. renewDeadline must be the deadline the elector uses.
func NewLease(lock resourcelock.Interface, renewDeadline time.Duration) *Lease {
	return &Lease{Interface: lock, renewDeadline: renewDeadline, now: time.Now}
}

// Create creates the lock record and records the result.
func (l *Lease) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	started := l.now()

	err := l.Interface.Create(ctx, record)
	l.record(started, record, err)

	return err
}

// Update updates the lock record and records the result. Releasing the
// Lease is an update with another (empty) holder, so it ends HoldsLease.
func (l *Lease) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	started := l.now()

	err := l.Interface.Update(ctx, record)
	l.record(started, record, err)

	return err
}

func (l *Lease) record(started time.Time, record resourcelock.LeaderElectionRecord, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err != nil {
		return
	}

	l.held = record.HolderIdentity == l.Identity()
	l.renewedAt = started
}

// HoldsLease reports whether this replica still holds the Lease.
func (l *Lease) HoldsLease() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.held && l.now().Sub(l.renewedAt) < l.renewDeadline
}

// ManagerOptions returns controller-runtime manager options that elect a
// leader through lease. The leader releases the Lease when its context
// ends. lease must have been created with timing.RenewDeadline.
func ManagerOptions(lease *Lease, timing ElectionTiming) manager.Options {
	return manager.Options{
		LeaderElection:                      true,
		LeaderElectionID:                    LeaseName,
		LeaderElectionResourceLockInterface: lease,
		LeaderElectionReleaseOnCancel:       true,
		LeaseDuration:                       &timing.LeaseDuration,
		RenewDeadline:                       &timing.RenewDeadline,
		RetryPeriod:                         &timing.RetryPeriod,
	}
}
