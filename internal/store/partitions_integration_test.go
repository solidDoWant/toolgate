//go:build integration

package store_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// insertAuditRow writes one audit row dated now, as the gateway does for
// every decision.
func insertAuditRow(ctx context.Context, pool *pgxpool.Pool) error {
	return db.New(pool).InsertAuditEvent(ctx, db.InsertAuditEventParams{Type: "tool.decision", Details: []byte("{}")})
}

// dropCurrentPartitions drops the partitions for the database's current and
// next month (in UTC). This is the state of a gateway that started two
// months ago and has run since: its partitions are for older months.
func dropCurrentPartitions(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	var current, next string
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY_MM'), to_char((now() AT TIME ZONE 'UTC') + interval '1 month', 'YYYY_MM')",
	).Scan(&current, &next))

	for _, month := range []string{current, next} {
		_, err := pool.Exec(t.Context(), "DROP TABLE IF EXISTS "+pgx.Identifier{"audit_events_" + month}.Sanitize())
		require.NoError(t, err)
	}

	require.Error(t, insertAuditRow(t.Context(), pool), "no partition holds this month's rows")
}

// syncBuffer is a log destination safe for the keeper's goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func runKeeper(t *testing.T, keeper *store.AuditPartitionKeeper) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- keeper.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func TestAuditPartitionKeeperCreatesPartitionsOnItsSchedule(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()

	// The leader's first call happens when it gets the Lease.
	dropCurrentPartitions(t, pool)

	keeper := store.NewAuditPartitionKeeper(pool, slog.New(slog.DiscardHandler))
	keeper.Interval = 200 * time.Millisecond
	runKeeper(t, keeper)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.NoError(c, insertAuditRow(ctx, pool))
	}, 30*time.Second, 50*time.Millisecond, "the call at the start creates this month's partition")

	// The clock passes into a month the gateway has no partition for,
	// without a restart. The next scheduled call creates it.
	dropCurrentPartitions(t, pool)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.NoError(c, insertAuditRow(ctx, pool))
	}, 30*time.Second, 50*time.Millisecond, "the scheduled call creates the partition again")

	assert.Zero(t, promtestutil.ToFloat64(keeper.Errors()))
}

func TestAuditPartitionKeeperLogsAndCountsFailures(t *testing.T) {
	pg, admin := migratedPool(t)
	ctx := t.Context()

	// A role without EXECUTE on the function: every call fails.
	_, err := admin.Exec(ctx, "CREATE ROLE no_execute LOGIN")
	require.NoError(t, err)

	certFile, keyFile := pg.IssueClientCert(t, "no_execute")
	pool := openPool(t, pg.ConnStringFor("no_execute", certFile, keyFile))

	logs := &syncBuffer{}
	keeper := store.NewAuditPartitionKeeper(pool, slog.New(slog.NewJSONHandler(logs, nil)))
	keeper.RetryDelay = 10 * time.Millisecond
	runKeeper(t, keeper)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.GreaterOrEqual(c, promtestutil.ToFloat64(keeper.Errors()), 2.0, "each failed attempt counts")
	}, 30*time.Second, 20*time.Millisecond, "a failed call is retried")

	assert.Contains(t, logs.String(), `"level":"ERROR"`)
	assert.Contains(t, logs.String(), "could not create the audit partitions")
	assert.Contains(t, logs.String(), "permission denied")
}

func TestAuditPartitionFunctionWorksForRoleWithoutDDLRights(t *testing.T) {
	pg, admin := migratedPool(t)
	ctx := t.Context()

	for _, statement := range []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"CREATE ROLE data_only LOGIN",
		"GRANT USAGE ON SCHEMA public TO data_only",
		"GRANT SELECT, INSERT ON audit_events TO data_only",
		"GRANT EXECUTE ON FUNCTION toolgate_ensure_audit_partitions() TO data_only",
	} {
		_, err := admin.Exec(ctx, statement)
		require.NoError(t, err, statement)
	}

	dropCurrentPartitions(t, admin)

	certFile, keyFile := pg.IssueClientCert(t, "data_only")
	pool := openPool(t, pg.ConnStringFor("data_only", certFile, keyFile))

	err := store.CreateAuditPartitions(ctx, pool, time.Now())
	require.ErrorContains(t, err, "permission denied", "the role can't create tables itself")

	require.NoError(t, store.EnsureAuditPartitions(ctx, pool))
	require.NoError(t, insertAuditRow(ctx, pool))
}

func TestAuditPartitionFunctionIgnoresSessionTimeZone(t *testing.T) {
	pg, pool := migratedPool(t)
	ctx := t.Context()

	// Partitions created by the Go code, in UTC, before the function
	// existed: last month's and this month's. The next month's partition is
	// missing, so the function creates it next to the Go one.
	var current, next string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY_MM'), to_char((now() AT TIME ZONE 'UTC') + interval '1 month', 'YYYY_MM')",
	).Scan(&current, &next))

	for _, month := range []string{current, next} {
		_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{"audit_events_" + month}.Sanitize())
		require.NoError(t, err)
	}

	require.NoError(t, store.CreateAuditPartitions(ctx, pool, time.Now().UTC().AddDate(0, -1, 0)))

	for _, zone := range []string{"Pacific/Kiritimati", "Pacific/Pago_Pago"} {
		t.Run(zone, func(t *testing.T) {
			conn, err := pgx.Connect(ctx, pg.ConnString())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close(context.Background()) })

			_, err = conn.Exec(ctx, "SET TimeZone = "+pgx.Identifier{zone}.Sanitize())
			require.NoError(t, err)

			_, err = conn.Exec(ctx, "SELECT toolgate_ensure_audit_partitions()")
			require.NoError(t, err, "the function's month bounds meet the Go partitions without overlap")
		})
	}

	// Every partition starts and ends at midnight UTC on the first of a month.
	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'FROM \(''([^'']+)''\)'))[1],
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'audit_events'::regclass`)
	require.NoError(t, err)

	type partition struct{ name, from, to string }

	partitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (partition, error) {
		var name, from, to string
		if err := row.Scan(&name, &from, &to); err != nil {
			return partition{}, err
		}

		return partition{name: name, from: from, to: to}, nil
	})
	require.NoError(t, err)
	require.Len(t, partitions, 3, "last month's and this month's from Go, next month's from the function")

	names := make([]string, 0, len(partitions))

	for _, p := range partitions {
		names = append(names, p.name)

		var from, to time.Time
		require.NoError(t, pool.QueryRow(ctx, "SELECT $1::timestamptz, $2::timestamptz", p.from, p.to).Scan(&from, &to))

		month, err := time.Parse("2006_01", p.name[len("audit_events_"):])
		require.NoError(t, err)
		assert.True(t, month.Equal(from), "%s starts at %s, want %s", p.name, from.UTC(), month)
		assert.True(t, month.AddDate(0, 1, 0).Equal(to), "%s ends at %s", p.name, to.UTC())
	}

	assert.Contains(t, names, "audit_events_"+next)
	require.NoError(t, insertAuditRow(ctx, pool))
}
