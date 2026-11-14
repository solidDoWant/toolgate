//go:build integration

package runtime_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// The exchange rows of a pending instance follow the generation the leader
// renders, and the starting write records each token's targets and times.
func TestPostgresStartStoreKeepsExchangeRows(t *testing.T) {
	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	_, err = queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: instanceID, TemplateNs: "catalog", TemplateName: "github", TemplateUid: templateUID,
		Generation: 1, Scope: "per-user", ScopeKey: "user:alice", OwnerPrincipal: "alice", Namespace: instanceNamespace,
	})
	require.NoError(t, err)

	for _, exchange := range []string{"removed", "temporal"} {
		require.NoError(t, queries.InsertInstanceExchange(t.Context(), db.InsertInstanceExchangeParams{
			InstanceID: instanceID, Exchange: exchange, Closed: true,
		}))
	}

	listener := store.NewListener(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	events, unsubscribe := listener.Subscribe()
	t.Cleanup(unsubscribe)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() { _ = listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	startStore := runtime.PostgresStartStore{Pool: pool}

	rows, err := startStore.SyncExchanges(t.Context(), instanceID, []string{"grafana", "temporal"}, []string{"grafana", "temporal"})
	require.NoError(t, err)
	expectEvent(t, events)

	require.Len(t, rows, 2)
	assert.Equal(t, "grafana", rows[0].Exchange)
	assert.Equal(t, "temporal", rows[1].Exchange)

	for _, row := range rows {
		assert.True(t, row.WantedSince.Valid, "%s waits for a token", row.Exchange)
		assert.False(t, row.ClaimedAt.Valid, "%s is open to any request's claim", row.Exchange)
		assert.WithinDuration(t, row.WantedSince.Time, row.RefreshAfter.Time, time.Second, "%s is due now", row.Exchange)
	}

	// Rows that already match change nothing and notify nobody: the next
	// notification is the one sent after the sync.
	_, err = startStore.SyncExchanges(t.Context(), instanceID, []string{"grafana", "temporal"}, []string{"temporal"})
	require.NoError(t, err)
	require.NoError(t, store.Notify(t.Context(), pool, store.KindSession, "after-sync"))

	select {
	case event := <-events:
		assert.Equal(t, store.Event{Kind: store.KindSession, ID: "after-sync"}, event)
	case <-time.After(30 * time.Second):
		t.Fatal("no notification within 30s")
	}

	issuedAt := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	expiresAt := issuedAt.Add(time.Hour)

	changed, err := startStore.MarkStarting(t.Context(), instanceID, 2, nil, time.Time{}, nil, []runtime.RenderedExchange{{
		Exchange: "temporal", IssuedAt: issuedAt, ExpiresAt: expiresAt,
		Targets: []runtime.ExchangeTarget{{Exchange: "temporal", Secret: "credentials", Key: "token"}},
	}})
	require.NoError(t, err)
	require.True(t, changed)
	expectEvent(t, events)

	instance, err := queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)
	assert.True(t, instance.UsesExchange, "the starting write records that the generation uses an exchange")

	rows, err = queries.ListInstanceExchanges(t.Context(), instanceID)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	temporal := rows[1]
	assert.True(t, temporal.IssuedAt.Time.Equal(issuedAt))
	assert.True(t, temporal.ExpiresAt.Time.Equal(expiresAt))
	assert.True(t, temporal.RefreshAfter.Time.Equal(issuedAt.Add(30*time.Minute)), "a token is refreshed at half-life")
	assert.False(t, temporal.WantedSince.Valid)
	assert.False(t, temporal.ClaimedAt.Valid)

	var targets []map[string]string
	require.NoError(t, json.Unmarshal(temporal.Targets, &targets))
	assert.Equal(t, []map[string]string{{"secret": "credentials", "key": "token"}}, targets)

	rows, err = startStore.SyncExchanges(t.Context(), instanceID, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, rows, "a generation without exchanges keeps no rows")
}

// Every replica listens on the credentials channel, and a subscriber gets
// each payload as it was sent.
func TestListenerPassesCredentialPayloads(t *testing.T) {
	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	listener := store.NewListener(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	payloads, unsubscribe := listener.SubscribeCredentials()
	t.Cleanup(unsubscribe)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() { _ = listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	require.NoError(t, db.New(pool).NotifyCredentials(t.Context(), `{"v":1}`))

	select {
	case payload := <-payloads:
		assert.JSONEq(t, `{"v":1}`, string(payload))
	case <-time.After(30 * time.Second):
		t.Fatal("no payload within 30s")
	}
}
