// Package changes tells subscribers that something they read has changed.
// A subscriber reads the current state again on each signal, so signals
// sent before it got round to reading are merged into one.
package changes

import "sync"

// Feed signals its subscribers. The zero value is ready to use and safe for
// concurrent use.
type Feed struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

// Subscribe returns a channel that receives after each Notify, and a
// function that ends the subscription. Signals not yet received are merged:
// the channel holds at most one.
func (f *Feed) Subscribe() (<-chan struct{}, func()) {
	signal := make(chan struct{}, 1)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.subscribers == nil {
		f.subscribers = make(map[chan struct{}]struct{})
	}

	f.subscribers[signal] = struct{}{}

	return signal, func() {
		f.mu.Lock()
		defer f.mu.Unlock()

		delete(f.subscribers, signal)
	}
}

// Notify signals every subscriber. It never blocks.
func (f *Feed) Notify() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for signal := range f.subscribers {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
}
