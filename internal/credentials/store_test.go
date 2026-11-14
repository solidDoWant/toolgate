package credentials_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/credentials"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func deliveryIssuedAt(issuedAt time.Time, token string) credentials.Delivery {
	return credentials.Delivery{
		Instance:  "inst-1",
		Exchange:  "temporal",
		Principal: "user:alice",
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt.Add(time.Hour),
		Token:     credentials.Token(token),
	}
}

func TestDeliveredTokensKeepNewestIssuedAt(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	store := &credentials.DeliveredTokens{Now: clock.Now}

	older := deliveryIssuedAt(clock.now.Add(-time.Minute), "older")
	newer := deliveryIssuedAt(clock.now, "newer")

	assert.True(t, store.Put(newer))
	assert.False(t, store.Put(older), "an older token doesn't replace a newer one")
	assert.False(t, store.Put(deliveryIssuedAt(clock.now, "same time")), "the same issue time doesn't replace")

	held, ok := store.Get("inst-1", "temporal")
	require.True(t, ok)
	assert.Equal(t, "newer", string(held.Token))

	newest := deliveryIssuedAt(clock.now.Add(time.Second), "newest")
	assert.True(t, store.Put(newest))

	held, ok = store.Get("inst-1", "temporal")
	require.True(t, ok)
	assert.Equal(t, "newest", string(held.Token))

	_, ok = store.Get("inst-1", "other")
	assert.False(t, ok, "the key includes the exchange")
	_, ok = store.Get("inst-2", "temporal")
	assert.False(t, ok, "the key includes the instance")
}

func TestDeliveredTokensDroppedAfterTwoMinutes(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	store := &credentials.DeliveredTokens{Now: clock.Now}

	require.True(t, store.Put(deliveryIssuedAt(clock.now, "token")))

	clock.now = clock.now.Add(2*time.Minute - time.Nanosecond)
	_, ok := store.Get("inst-1", "temporal")
	assert.True(t, ok, "held until 2 minutes have passed")

	clock.now = clock.now.Add(time.Nanosecond)
	_, ok = store.Get("inst-1", "temporal")
	assert.False(t, ok, "dropped at 2 minutes")

	t.Run("an older token is kept after the newer one expired", func(t *testing.T) {
		start := clock.now
		require.True(t, store.Put(deliveryIssuedAt(start, "newer")))

		clock.now = start.Add(2 * time.Minute)
		assert.True(t, store.Put(deliveryIssuedAt(start.Add(-time.Minute), "older")))
	})
}

func TestDeliveredTokensForgetOnApply(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	store := &credentials.DeliveredTokens{Now: clock.Now}

	applied := deliveryIssuedAt(clock.now, "applied")
	require.True(t, store.Put(applied))

	newer := deliveryIssuedAt(clock.now.Add(time.Second), "newer")
	require.True(t, store.Put(newer))

	store.Forget("inst-1", "temporal", applied.IssuedAt)
	held, ok := store.Get("inst-1", "temporal")
	require.True(t, ok, "a newer token that arrived during the apply is kept")
	assert.Equal(t, "newer", string(held.Token))

	store.Forget("inst-1", "temporal", newer.IssuedAt)
	_, ok = store.Get("inst-1", "temporal")
	assert.False(t, ok)
}
