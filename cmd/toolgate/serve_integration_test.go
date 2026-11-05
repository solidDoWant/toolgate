//go:build integration

package main

import (
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// startServe runs `toolgate serve` against the database at databaseURL,
// with a fake identity provider. It returns the gateway's output and
// its exit code channel, once the gateway logged that it started.
// Several tests here run serve, and so its controllers, in this one process.
func init() { skipControllerNameValidation = true }

func startServe(t *testing.T, databaseURL string) (*lockedBuffer, *lockedBuffer, chan int) {
	t.Helper()
	clearConfigEnv(t)

	return startServeWith(t, databaseURL, testutil.StartOIDCProvider(t, testutil.OIDCOptions{}))
}

// startServeWith is startServe with the given identity provider and the
// rest of the configuration environment left as the caller set it.
func startServeWith(t *testing.T, databaseURL string, provider *testutil.OIDCProvider) (*lockedBuffer, *lockedBuffer, chan int) {
	t.Helper()
	t.Setenv("TOOLGATE_LISTEN_ADDRESS", "127.0.0.1:0")
	t.Setenv("TOOLGATE_METRICS_LISTEN_ADDRESS", "127.0.0.1:0")
	t.Setenv("TOOLGATE_DATABASE_URL", databaseURL)
	t.Setenv("TOOLGATE_OIDC_ISSUER_URL", provider.Issuer)
	t.Setenv("TOOLGATE_OIDC_AUDIENCE", provider.Audience)

	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}
	done := make(chan int, 1)

	go func() { done <- run([]string{"serve"}, stdout, stderr) }()

	require.Eventually(t, func() bool {
		select {
		case code := <-done:
			done <- code

			return true
		default:
			return strings.Contains(stdout.String(), `"msg":"gateway started"`)
		}
	}, 2*time.Minute, 10*time.Millisecond, "the gateway did not start")

	require.Contains(t, stdout.String(), `"msg":"gateway started"`, "stderr: %s", stderr.String())

	return stdout, stderr, done
}

// stopServe sends SIGTERM and asserts that serve exits with 0.
func stopServe(t *testing.T, stderr *lockedBuffer, done chan int) {
	t.Helper()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case code := <-done:
		assert.Equal(t, 0, code, "stderr: %s", stderr.String())
	case <-time.After(30 * time.Second):
		require.FailNow(t, "serve did not stop after SIGTERM")
	}
}

func openPool(t *testing.T, connString string) *pgxpool.Pool {
	t.Helper()

	pool, err := store.Open(t.Context(), connString)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func TestServeStopsOnSIGTERM(t *testing.T) {
	stdout, stderr, done := startServe(t, testutil.StartPostgres(t).ConnString())

	stopServe(t, stderr, done)

	assert.Contains(t, stdout.String(), `"msg":"gateway stopped"`)
}

func TestServeAppliesTheMigrationsBeforeItListens(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())

	var tables int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_tables WHERE schemaname = 'public'").Scan(&tables))
	require.Zero(t, tables, "the database starts empty")

	_, stderr, done := startServe(t, pg.ConnString())

	// The listeners are open now, so the schema must already exist.
	for _, table := range []string{"instances", "user_secrets", "audit_events"} {
		var name *string
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT to_regclass($1)::text", table).Scan(&name))
		assert.NotNil(t, name, "table %s exists before the gateway serves", table)
	}

	stopServe(t, stderr, done)
}

func TestServeStartsAndWarnsWhenTheDatabaseSchemaIsNewer(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	require.NoError(t, store.Migrate(t.Context(), pool))

	// A newer release applied a migration this binary doesn't know.
	const futureVersion = int64(99991231000000)

	_, err := pool.Exec(t.Context(),
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)", futureVersion)
	require.NoError(t, err)

	stdout, stderr, done := startServe(t, pg.ConnString())

	type logEntry struct {
		Level           string
		DatabaseVersion int64
	}

	var warning logEntry

	for line := range strings.SplitSeq(stdout.String(), "\n") {
		var entry logEntry
		if json.Unmarshal([]byte(line), &entry) == nil && entry.DatabaseVersion != 0 {
			warning = entry
		}
	}

	assert.Equal(t, "WARN", warning.Level, "stdout: %s", stdout.String())
	assert.Equal(t, futureVersion, warning.DatabaseVersion)

	stopServe(t, stderr, done)

	var applied bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT is_applied FROM goose_db_version WHERE version_id = $1", futureVersion).Scan(&applied))
	assert.True(t, applied, "the gateway never migrates down")
}
