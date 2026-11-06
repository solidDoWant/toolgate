//go:build integration

package store_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3/lock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func openPool(t *testing.T, connString string) *pgxpool.Pool {
	t.Helper()

	pool, err := store.Open(t.Context(), connString)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func migratedPool(t *testing.T) (*testutil.Postgres, *pgxpool.Pool) {
	t.Helper()

	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	require.NoError(t, store.Migrate(t.Context(), pool))

	return pg, pool
}

func TestMigrateCreatesSchema(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()

	for _, table := range []string{"sessions", "session_servers", "instances", "user_secrets", "audit_events"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists))
		assert.True(t, exists, "table %s", table)
	}

	var strategy string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT partstrat::text FROM pg_partitioned_table WHERE partrelid = 'audit_events'::regclass").Scan(&strategy))
	assert.Equal(t, "r", strategy, "audit_events is range-partitioned")

	month := time.Now().UTC()
	thisMonth := "audit_events_" + month.Format("2006_01")
	nextMonth := "audit_events_" + time.Date(month.Year(), month.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format("2006_01")

	for _, partition := range []string{thisMonth, nextMonth} {
		var bound string
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE relname = $1", partition).Scan(&bound), partition)
		assert.Contains(t, bound, "FOR VALUES FROM", partition)
	}

	queries := db.New(pool)
	require.NoError(t, queries.InsertAuditEvent(ctx, db.InsertAuditEventParams{Type: "session.start", Details: []byte("{}")}))

	var partition string
	require.NoError(t, pool.QueryRow(ctx, "SELECT tableoid::regclass::text FROM audit_events").Scan(&partition))
	assert.Equal(t, thisMonth, partition)
}

func TestLiveInstanceUniqueIndex(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)

	insert := func(id, scopeKey string) error {
		_, err := queries.InsertInstance(ctx, db.InsertInstanceParams{
			ID: id, TemplateNs: "catalog", TemplateName: "github", TemplateUid: "uid-1", Generation: 1,
			Scope: "per-user", ScopeKey: scopeKey, OwnerPrincipal: "alice", Namespace: "tg-" + id,
		})

		return err
	}

	const first, second, third = "0000000000000000000000000000000a", "0000000000000000000000000000000b", "0000000000000000000000000000000c"

	require.NoError(t, insert(first, "user:alice"))
	require.NoError(t, insert(third, "user:bob"), "another scope key is a different live instance")

	var pgErr *pgconn.PgError

	err := insert(second, "user:alice")
	require.ErrorAs(t, err, &pgErr, "a second live instance for the same template and scope key")
	assert.Equal(t, "23505", pgErr.Code)

	rows, err := queries.SetInstanceFailed(ctx, db.SetInstanceFailedParams{ID: first, FromStatus: "pending", Reason: "render failed"})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	require.NoError(t, insert(second, "user:alice"), "a failed instance is not live")
}

func TestMigrateConcurrently(t *testing.T) {
	pg := testutil.StartPostgres(t)

	const gateways = 5

	pools := make([]*pgxpool.Pool, gateways)
	for index := range pools {
		pools[index] = openPool(t, pg.ConnString())
	}

	start := make(chan struct{})
	errs := make([]error, gateways)

	var wait sync.WaitGroup
	for index, pool := range pools {
		wait.Go(func() {
			<-start

			errs[index] = store.Migrate(t.Context(), pool)
		})
	}

	close(start)
	wait.Wait()

	for index, err := range errs {
		require.NoError(t, err, "gateway %d", index)
	}

	files, err := filepath.Glob("migrations/*.sql")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	var applied, distinct int
	require.NoError(t, pools[0].QueryRow(t.Context(),
		"SELECT count(*), count(DISTINCT version_id) FROM goose_db_version WHERE version_id > 0").Scan(&applied, &distinct))
	assert.Equal(t, len(files), distinct, "every migration is applied")
	assert.Equal(t, len(files), applied, "each migration is applied once")
}

func TestMigrateWaitsForAnotherGatewaysMigration(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := t.Context()

	// Hold the migration lock as another gateway mid-migration would.
	holder, err := pgx.Connect(ctx, pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close(context.WithoutCancel(ctx)) })

	_, err = holder.Exec(ctx, "SELECT pg_advisory_lock($1)", lock.DefaultLockID)
	require.NoError(t, err)

	pool := openPool(t, pg.ConnString())
	done := make(chan error, 1)

	go func() { done <- store.Migrate(ctx, pool) }()

	select {
	case err := <-done:
		t.Fatalf("Migrate returned while another gateway held the migration lock: %v", err)
	case <-time.After(2 * time.Second):
	}

	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('public.sessions') IS NOT NULL").Scan(&exists))
	require.False(t, exists, "no migration is applied while the lock is held")

	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock.DefaultLockID)
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Minute):
		t.Fatal("Migrate did not finish after the lock was released")
	}
}

func TestNotifyReachesOtherConnections(t *testing.T) {
	_, pool := migratedPool(t)

	events, stop := startListener(t, pool)
	defer stop()

	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		return store.Notify(t.Context(), tx, store.KindInstance, "0123")
	})
	require.NoError(t, err)

	assert.Equal(t, store.Event{Kind: store.KindInstance, ID: "0123"}, nextEvent(t, events))

	// A rolled-back transaction delivers nothing; the next commit arrives
	// alone.
	err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		require.NoError(t, store.Notify(t.Context(), tx, store.KindSession, "1"))

		return errors.New("roll back")
	})
	require.Error(t, err)
	require.NoError(t, store.Notify(t.Context(), pool, store.KindSession, "2"))

	assert.Equal(t, store.Event{Kind: store.KindSession, ID: "2"}, nextEvent(t, events))
}

func TestListenerReconnectsAndReportsMissed(t *testing.T) {
	_, pool := migratedPool(t)

	events, stop := startListener(t, pool)
	defer stop()

	ctx := t.Context()

	reconnects := opsmetrics.Value("toolgate_notify_reconnects_total", nil)

	var terminated int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN%' AND pid <> pg_backend_pid()) AS t").Scan(&terminated))
	require.Equal(t, 1, terminated, "the listener's connection")

	assert.Equal(t, store.Event{Missed: true}, nextEvent(t, events))
	assert.InDelta(t, reconnects+1, opsmetrics.Value("toolgate_notify_reconnects_total", nil), 0)

	require.NoError(t, store.Notify(ctx, pool, store.KindSession, "7"))
	assert.Equal(t, store.Event{Kind: store.KindSession, ID: "7"}, nextEvent(t, events))
}

func TestNewConnectionsUseRotatedClientCertificate(t *testing.T) {
	pg := testutil.StartPostgres(t)
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, pg.ConnString())
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "CREATE ROLE rotated LOGIN")
	require.NoError(t, err)
	require.NoError(t, conn.Close(ctx))

	dir := t.TempDir()
	certFile, keyFile := dir+"/client.crt", dir+"/client.key"
	copyFile(t, pg.CertFile, certFile)
	copyFile(t, pg.KeyFile, keyFile)

	// The connection string names the role the rotated certificate is
	// for, so only the rotated files can authenticate.
	pool := openPool(t, pg.ConnStringFor("rotated", certFile, keyFile))

	err = pool.Ping(ctx)
	require.Error(t, err, "the original certificate is for another user")

	rotatedCert, rotatedKey := pg.IssueClientCert(t, "rotated")
	copyFile(t, rotatedCert, certFile)
	copyFile(t, rotatedKey, keyFile)

	var user string
	require.NoError(t, pool.QueryRow(ctx, "SELECT current_user").Scan(&user))
	assert.Equal(t, "rotated", user)
}

func startListener(t *testing.T, pool *pgxpool.Pool) (<-chan store.Event, func()) {
	t.Helper()

	listener := store.NewListener(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	events, unsubscribe := listener.Subscribe()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	return events, func() {
		unsubscribe()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	}
}

func nextEvent(t *testing.T, events <-chan store.Event) store.Event {
	t.Helper()

	select {
	case event := <-events:
		return event
	case <-time.After(30 * time.Second):
		t.Fatal("no event within 30s")

		return store.Event{}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()

	content, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, content, 0o600))
}
