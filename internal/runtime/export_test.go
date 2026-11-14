package runtime

import (
	"context"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// NewLeaseWithClock is NewLease with a clock the test controls.
func NewLeaseWithClock(lock resourcelock.Interface, renewDeadline time.Duration, now func() time.Time) *Lease {
	lease := NewLease(lock, renewDeadline)
	lease.now = now

	return lease
}

// SnapshotIdentity is snapshotIdentity, the identity the reconciler's access
// re-check judges.
var SnapshotIdentity = snapshotIdentity

// RenderIdentity is the identity the starter renders the instance with.
func (s *Starter) RenderIdentity(ctx context.Context, instance db.Instance) (Identity, error) {
	return s.identity(ctx, instance)
}

// OrderForApply is the order applyAll applies rendered objects in.
var OrderForApply = orderForApply

// Volume helpers and their results, for the volume tests.
var (
	CheckStorageClass   = checkStorageClass
	CheckRecordedVolume = checkRecordedVolume
	PreBind             = preBind
	RecordVolume        = recordVolume
)

// The outcomes of a volume step.
const (
	VolumeGoOn = volumeGoOn
	VolumeWait = volumeWait
	VolumeFail = volumeFail
	VolumeLost = volumeLost
)

// VolumeStep is the result of a volume check.
type VolumeStep = volumeStep
