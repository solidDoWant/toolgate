//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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
func init() {
	skipControllerNameValidation = true
	auditTailPollInterval = 50 * time.Millisecond
	auditTailSettle = time.Second
}

func startServe(t *testing.T, databaseURL string) (*lockedBuffer, *lockedBuffer, *serveRun) {
	t.Helper()

	return startServeWith(t, testEnv{}, databaseURL, testutil.StartOIDCProvider(t, testutil.OIDCOptions{}))
}

// startServeWith is startServe with the given identity provider and the
// rest of the configuration environment left as the caller set it.
func startServeWith(t *testing.T, env testEnv, databaseURL string, provider *testutil.OIDCProvider) (*lockedBuffer, *lockedBuffer, *serveRun) {
	t.Helper()
	env.set("TOOLGATE_LISTEN_ADDRESS", "127.0.0.1:0")
	env.set("TOOLGATE_METRICS_LISTEN_ADDRESS", "127.0.0.1:0")
	env.set("TOOLGATE_DATABASE_URL", databaseURL)
	env.set("TOOLGATE_OIDC_ISSUER_URL", provider.Issuer)

	if env.get("TOOLGATE_OIDC_AUDIENCE") == "" {
		env.set("TOOLGATE_OIDC_AUDIENCE", provider.Audience)
	}

	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}

	return startServeCommand(t, env, stdout, stderr)
}

// serveRun is a running `toolgate serve`: the channel gets its exit code and
// stop ends it the way SIGTERM does.
type serveRun struct {
	done chan int
	stop context.CancelFunc
}

// startServeCommand runs `toolgate serve` with env until the test stops it,
// and waits until the gateway logged that it started. A test that does not
// stop it explicitly stops it when the test ends.
func startServeCommand(t *testing.T, env testEnv, stdout, stderr *lockedBuffer) (*lockedBuffer, *lockedBuffer, *serveRun) {
	t.Helper()

	ctx, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)

	done := make(chan int, 1)

	go func() { done <- runContext(ctx, env.get, []string{"serve"}, stdout, stderr) }()

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

	return stdout, stderr, &serveRun{done: done, stop: stop}
}

// stopServe ends serve and asserts that it exits with 0.
func stopServe(t *testing.T, stderr *lockedBuffer, run *serveRun) {
	t.Helper()
	run.stop()

	select {
	case code := <-run.done:
		assert.Equal(t, 0, code, "stderr: %s", stderr.String())
	case <-time.After(30 * time.Second):
		require.FailNow(t, "serve did not stop when its context ended")
	}
}

func openPool(t *testing.T, connString string) *pgxpool.Pool {
	t.Helper()

	pool, err := store.Open(t.Context(), connString)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func TestServeStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()

	stdout, stderr, done := startServe(t, testutil.StartPostgres(t).ConnString())

	stopServe(t, stderr, done)

	assert.Contains(t, stdout.String(), `"msg":"gateway stopped"`)
}

func TestServeProcessStopsCleanlyOnSIGTERM(t *testing.T) {
	t.Parallel()

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})

	command := exec.CommandContext(t.Context(), os.Args[0], "serve")
	command.Env = append(os.Environ(),
		runAsBinaryEnv+"=1",
		"TOOLGATE_LISTEN_ADDRESS=127.0.0.1:0",
		"TOOLGATE_METRICS_LISTEN_ADDRESS=127.0.0.1:0",
		"TOOLGATE_DATABASE_URL="+testutil.StartPostgres(t).ConnString(),
		"TOOLGATE_OIDC_ISSUER_URL="+provider.Issuer,
		"TOOLGATE_OIDC_AUDIENCE="+provider.Audience,
	)

	stdout := &lockedBuffer{}
	command.Stdout = stdout
	command.Stderr = stdout

	require.NoError(t, command.Start())

	waited := make(chan error, 1)

	go func() { waited <- command.Wait() }()

	require.Eventually(t, func() bool { return strings.Contains(stdout.String(), `"msg":"gateway started"`) },
		2*time.Minute, 10*time.Millisecond, "the gateway did not start: %s", stdout.String())

	require.NoError(t, command.Process.Signal(syscall.SIGTERM))

	select {
	case err := <-waited:
		require.NoError(t, err, "the process exits 0 after SIGTERM; output: %s", stdout.String())
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the process did not stop after SIGTERM", "output: %s", stdout.String())
	}

	assert.Contains(t, stdout.String(), `"msg":"gateway stopped"`)
}

func TestServeAppliesTheMigrationsBeforeItListens(t *testing.T) {
	t.Parallel()

	pg := testutil.StartPostgresEmpty(t)
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
	t.Parallel()

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
