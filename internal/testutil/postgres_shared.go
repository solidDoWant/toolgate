package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
)

// Template databases the shared server keeps. The migrated one has every
// migration applied; the empty one is a copy of PostgreSQL's own template.
const (
	sharedMigratedTemplate = "toolgate_template"
	sharedEmptyTemplate    = "template1"

	// maintenanceDatabase is the database the helper connects to when it
	// creates and drops the per-test databases.
	maintenanceDatabase = "postgres"

	databasePrefix = "t_"
)

// sharedServer is the one PostgreSQL server of this test binary. It starts on
// the first StartPostgres call. Its container is removed by the testcontainers
// reaper when the binary exits.
var sharedServer struct {
	once sync.Once
	pg   *Postgres
	err  error

	templateOnce sync.Once
	templateErr  error

	namesMu sync.Mutex
	names   map[string]bool
}

func startSharedDatabase(tb testing.TB, template string) *Postgres {
	tb.Helper()
	RequireDocker(tb)

	sharedServer.once.Do(func() {
		sharedServer.pg, _, sharedServer.err = startPostgresServer(context.Background(), nil)
	})
	require.NoError(tb, sharedServer.err, "start PostgreSQL")

	if template == sharedMigratedTemplate {
		sharedServer.templateOnce.Do(func() {
			sharedServer.templateErr = createMigratedTemplate(context.Background(), sharedServer.pg)
		})
		require.NoError(tb, sharedServer.templateErr, "migrate the template database")
	}

	name, err := newDatabaseName()
	require.NoError(tb, err)
	require.Truef(tb, claimDatabaseName(name), "two tests got the database name %s", name)

	server := sharedServer.pg
	admin := server.inDatabase(maintenanceDatabase)

	execAdmin(tb, admin, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize()))

	tb.Cleanup(func() {
		// The test's context is already cancelled here, so use a fresh one.
		ctx := context.WithoutCancel(tb.Context())

		conn, err := pgx.Connect(ctx, admin.ConnString())
		if err != nil {
			tb.Errorf("connect to drop database %s: %v", name, err)

			return
		}

		defer func() { _ = conn.Close(ctx) }()

		if _, err := conn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pgx.Identifier{name}.Sanitize())); err != nil {
			tb.Errorf("drop database %s: %v", name, err)
		}
	})

	return server.inDatabase(name)
}

// createMigratedTemplate creates the template database and applies every
// migration to it.
func createMigratedTemplate(ctx context.Context, server *Postgres) error {
	maintenance, err := pgx.Connect(ctx, server.inDatabase(maintenanceDatabase).ConnString())
	if err != nil {
		return err
	}

	_, err = maintenance.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{sharedMigratedTemplate}.Sanitize())
	_ = maintenance.Close(ctx)

	if err != nil {
		return err
	}

	pool, err := store.Open(ctx, server.inDatabase(sharedMigratedTemplate).ConnString())
	if err != nil {
		return err
	}

	// Closing the pool ends every connection, which CREATE DATABASE needs
	// when it copies the template.
	defer pool.Close()

	return store.Migrate(ctx, pool)
}

func execAdmin(tb testing.TB, admin *Postgres, statement string) {
	tb.Helper()

	conn, err := pgx.Connect(tb.Context(), admin.ConnString())
	require.NoError(tb, err)

	defer func() { _ = conn.Close(context.WithoutCancel(tb.Context())) }()

	_, err = conn.Exec(tb.Context(), statement)
	require.NoError(tb, err, statement)
}

// inDatabase returns a copy of p that points at another database on the same
// server.
func (p *Postgres) inDatabase(name string) *Postgres {
	clone := *p
	clone.Database = name

	return &clone
}

// RoleName returns name with this test's prefix. Roles belong to the whole
// server, so tests on the shared server must name the roles they create with
// it.
func (p *Postgres) RoleName(name string) string {
	return p.Database + "_" + name
}

func newDatabaseName() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}

	return databasePrefix + hex.EncodeToString(random), nil
}

// claimDatabaseName records name as used. It reports false when another test
// of this binary already has it.
func claimDatabaseName(name string) bool {
	sharedServer.namesMu.Lock()
	defer sharedServer.namesMu.Unlock()

	if sharedServer.names == nil {
		sharedServer.names = map[string]bool{}
	}

	if sharedServer.names[name] {
		return false
	}

	sharedServer.names[name] = true

	return true
}
