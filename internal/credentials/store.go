package credentials

import (
	"sync"
	"time"
)

// DeliveredTokenHold is the longest the leader keeps a delivered token that
// it hasn't applied.
const DeliveredTokenHold = 2 * time.Minute

type deliveryKey struct {
	instance string
	exchange string
}

type heldDelivery struct {
	delivery   Delivery
	receivedAt time.Time
}

// DeliveredTokens is the leader's memory of delivered tokens, keyed by
// instance and exchange. It keeps only the newest token for a key, and only
// until the token is applied or DeliveredTokenHold has passed.
type DeliveredTokens struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mutex   sync.Mutex
	entries map[deliveryKey]heldDelivery
}

func (s *DeliveredTokens) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// Put keeps delivery unless the store holds a token with the same or a later
// issue time for its key. It reports whether it kept delivery.
func (s *DeliveredTokens) Put(delivery Delivery) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	now := s.now()
	s.dropExpired(now)

	key := deliveryKey{instance: delivery.Instance, exchange: delivery.Exchange}
	if held, ok := s.entries[key]; ok && !delivery.IssuedAt.After(held.delivery.IssuedAt) {
		return false
	}

	if s.entries == nil {
		s.entries = map[deliveryKey]heldDelivery{}
	}

	s.entries[key] = heldDelivery{delivery: delivery, receivedAt: now}

	return true
}

// Get returns the held token of an instance's exchange, if it is still held.
func (s *DeliveredTokens) Get(instance, exchange string) (Delivery, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.dropExpired(s.now())

	held, ok := s.entries[deliveryKey{instance: instance, exchange: exchange}]

	return held.delivery, ok
}

// Forget drops the held token of an instance's exchange once it is applied.
// It drops it only if its issue time is issuedAt, so a newer delivery that
// arrived during the apply is kept.
func (s *DeliveredTokens) Forget(instance, exchange string, issuedAt time.Time) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	key := deliveryKey{instance: instance, exchange: exchange}
	if held, ok := s.entries[key]; ok && held.delivery.IssuedAt.Equal(issuedAt) {
		delete(s.entries, key)
	}
}

func (s *DeliveredTokens) dropExpired(now time.Time) {
	for key, held := range s.entries {
		if now.Sub(held.receivedAt) >= DeliveredTokenHold {
			delete(s.entries, key)
		}
	}
}
