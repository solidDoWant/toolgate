package embeddings

import (
	"errors"
	"sync"
	"time"
)

// CircuitOpenFor is how long a client skips its endpoint after a call fails.
const CircuitOpenFor = 30 * time.Second

// ErrCircuitOpen means the endpoint failed lately and the call was skipped.
var ErrCircuitOpen = errors.New("the endpoint failed lately and is skipped for a while")

// ErrDimensionMismatch means stored vectors have another length than the
// vectors the endpoint returns now.
var ErrDimensionMismatch = errors.New("stored vectors differ in length from the query vector")

// breaker skips an endpoint for CircuitOpenFor after a call fails, so a
// single outage does not add the timeout to every search.
type breaker struct {
	mutex sync.Mutex
	until time.Time
	now   func() time.Time
}

// allow reports whether a call may go out.
func (b *breaker) allow() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return !b.now().Before(b.until)
}

// record opens the circuit after a failure and closes it after a success.
func (b *breaker) record(failed bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if failed {
		b.until = b.now().Add(CircuitOpenFor)
	} else {
		b.until = time.Time{}
	}
}
