package runtime

import (
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// NewLeaseWithClock is NewLease with a clock the test controls.
func NewLeaseWithClock(lock resourcelock.Interface, renewDeadline time.Duration, now func() time.Time) *Lease {
	lease := NewLease(lock, renewDeadline)
	lease.now = now

	return lease
}
