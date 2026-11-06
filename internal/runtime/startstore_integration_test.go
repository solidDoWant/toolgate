//go:build integration

package runtime_test

import (
	"context"
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

// The PostgreSQL start store's writes are compare-and-sets that notify only
// when they change the row: a late ready after a timeout changes nothing.
func TestPostgresStartStoreCompareAndSet(t *testing.T) {
	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	_, err = queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: instanceID, TemplateNs: "catalog", TemplateName: "github", TemplateUid: templateUID,
		Generation: 1, Scope: "shared", ScopeKey: "shared", Namespace: instanceNamespace,
	})
	require.NoError(t, err)

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

	changed, err := startStore.MarkStarting(t.Context(), instanceID, 4, nil, time.Time{})
	require.NoError(t, err)
	assert.True(t, changed)
	expectEvent(t, events)

	changed, err = startStore.MarkFailed(t.Context(), instanceID, "starting", "timed out")
	require.NoError(t, err)
	assert.True(t, changed)
	expectEvent(t, events)

	changed, err = startStore.MarkReady(t.Context(), db.Instance{ID: instanceID}, []byte(`[]`), runtime.RevisionStateful)
	require.NoError(t, err)
	assert.False(t, changed, "a late ready after the timeout changes nothing")

	row, err := queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, "failed", row.Status)
	assert.Equal(t, "timed out", row.Reason)
	assert.Equal(t, int64(4), row.Generation)
	assert.Nil(t, row.Tools)

	select {
	case event := <-events:
		t.Fatalf("unexpected notification %+v", event)
	case <-time.After(time.Second):
	}
}

func expectEvent(t *testing.T, events <-chan store.Event) {
	t.Helper()

	select {
	case event := <-events:
		assert.Equal(t, store.Event{Kind: store.KindInstance, ID: instanceID}, event)
	case <-time.After(30 * time.Second):
		t.Fatal("no notification within 30s")
	}
}

// A pending instance's used secrets are stored with the time each was set,
// and read back unchanged.
func TestPostgresStartStoreRecordsSecretsUsed(t *testing.T) {
	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	_, err = queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: instanceID, TemplateNs: "catalog", TemplateName: "github", TemplateUid: templateUID,
		Generation: 1, Scope: "per-user", ScopeKey: "alice", OwnerPrincipal: "alice", Namespace: instanceNamespace,
	})
	require.NoError(t, err)

	setAt := time.Date(2030, 1, 2, 3, 4, 5, 6000, time.UTC)
	require.NoError(t, runtime.PostgresStartStore{Pool: pool}.RecordSecretsUsed(t.Context(), instanceID, map[string]time.Time{"token": setAt}))

	row, err := queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)

	used, err := runtime.DecodeSecretsUsed(row.SecretsUsed)
	require.NoError(t, err)
	require.Contains(t, used, "token")
	assert.True(t, setAt.Equal(used["token"]))
}

// The ready write stores the instance's MCP revision and records it in
// instance.ready. Later writes of the ready row keep it.
func TestPostgresStartStoreMarkReadyStoresTheRevision(t *testing.T) {
	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)
	instance, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: instanceID, TemplateNs: "catalog", TemplateName: "github", TemplateUid: templateUID,
		Generation: 1, Scope: "shared", ScopeKey: "shared", Namespace: instanceNamespace,
	})
	require.NoError(t, err)

	startStore := runtime.PostgresStartStore{Pool: pool}
	_, err = startStore.MarkStarting(t.Context(), instanceID, 1, nil, time.Time{})
	require.NoError(t, err)

	changed, err := startStore.MarkReady(t.Context(), instance, []byte(`[]`), runtime.RevisionStateless)
	require.NoError(t, err)
	assert.True(t, changed)

	changed, err = startStore.MarkReady(t.Context(), instance, []byte(`[]`), runtime.RevisionStateful)
	require.NoError(t, err)
	assert.False(t, changed, "a ready row is never made ready again")

	_, err = startStore.SetTools(t.Context(), instanceID, []byte(`[{"name":"search"}]`))
	require.NoError(t, err)

	row, err := queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, "ready", row.Status)
	assert.Equal(t, runtime.RevisionStateless, row.McpRevision)

	var revisions []string

	rows, err := pool.Query(t.Context(),
		`SELECT details->>'mcpRevision' FROM audit_events WHERE type = 'instance.ready' AND details->>'instance' = $1`, instanceID)
	require.NoError(t, err)

	for rows.Next() {
		var revision string
		require.NoError(t, rows.Scan(&revision))
		revisions = append(revisions, revision)
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, []string{runtime.RevisionStateless}, revisions)
}
