//go:build integration

package store_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
)

func TestReplyNotificationReachesReplySubscribersOnly(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()

	listener := store.NewListener(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	events, unsubscribe := listener.Subscribe()
	replies, unsubscribeReplies := listener.SubscribeReplies()

	runCtx, cancel := context.WithCancel(ctx)

	defer func() {
		unsubscribe()
		unsubscribeReplies()
		cancel()
	}()

	go func() { _ = listener.Run(runCtx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	largest := []byte(strings.Repeat("x", store.MaxReplyBytes))
	require.NoError(t, store.NotifyReply(ctx, pool, largest))

	select {
	case payload := <-replies:
		assert.Equal(t, largest, payload)
	case <-time.After(10 * time.Second):
		t.Fatal("no reply arrived")
	}

	require.ErrorIs(t, store.NotifyReply(ctx, pool, append(largest, 'x')), store.ErrReplyTooLarge)

	// A reply isn't an event: the next event is the one sent after it.
	require.NoError(t, store.Notify(ctx, pool, store.KindSession, "3"))
	assert.Equal(t, store.Event{Kind: store.KindSession, ID: "3"}, nextEvent(t, events))
}
