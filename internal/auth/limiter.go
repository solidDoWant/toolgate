package auth

import (
	"sync"
	"time"
)

// Failed sign-ins are recorded at most refusalLimit times per refusalWindow
// for each remote address (docs/design/phase-1.md §7).
const (
	refusalLimit  = 10
	refusalWindow = time.Minute

	// maxTrackedAddresses bounds the limiter's memory. When it is full, old
	// windows are removed; if all are current, new addresses are dropped.
	maxTrackedAddresses = 10000
)

// refusalLimiter counts refusals per remote address in fixed one-minute
// windows.
type refusalLimiter struct {
	mutex   sync.Mutex
	now     func() time.Time
	windows map[string]refusalCount
}

type refusalCount struct {
	start time.Time
	count int
}

func newRefusalLimiter() *refusalLimiter {
	return &refusalLimiter{now: time.Now, windows: map[string]refusalCount{}}
}

// allow reports whether another refusal from address may be recorded.
func (l *refusalLimiter) allow(address string) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	now := l.now()

	window, ok := l.windows[address]
	if !ok || now.Sub(window.start) >= refusalWindow {
		if !ok && len(l.windows) >= maxTrackedAddresses {
			l.prune(now)

			if len(l.windows) >= maxTrackedAddresses {
				return false
			}
		}

		l.windows[address] = refusalCount{start: now, count: 1}

		return true
	}

	if window.count >= refusalLimit {
		return false
	}

	window.count++
	l.windows[address] = window

	return true
}

func (l *refusalLimiter) prune(now time.Time) {
	for address, window := range l.windows {
		if now.Sub(window.start) >= refusalWindow {
			delete(l.windows, address)
		}
	}
}
