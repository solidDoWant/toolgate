//go:build integration

package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// phase2Version is the last migration a 0.2 gateway knows.
const phase2Version = 5

// insertPhase2Session inserts a session the way a 0.2 gateway does, without
// a kind.
const insertPhase2Session = `INSERT INTO sessions (principal, session_id, workspace, attestation) VALUES ($1, $2, 'w', $3)`

func TestScopingMigrationBackfillsSessionKind(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	ctx := t.Context()

	sqlDB := stdlib.OpenDBFromPool(pool)

	t.Cleanup(func() { _ = sqlDB.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, phase2Version)
	require.NoError(t, err)

	cases := []struct {
		principal   string
		attestation string
		wantKind    string
	}{
		{"alice", "user", "user"},
		{"spiffe://example.com/user-shaped", "user", "user"},
		{"system:serviceaccount:team:user-shaped", "user", "user"},
		{"spiffe://example.com/workload", "attested", "spiffe"},
		{"system:serviceaccount:team:bot", "attested", "service-account"},
	}

	for _, test := range cases {
		_, err := pool.Exec(ctx, insertPhase2Session, test.principal, "before", test.attestation)
		require.NoError(t, err)
	}

	require.NoError(t, store.Migrate(ctx, pool))

	queries := db.New(pool)

	for _, test := range cases {
		session, err := queries.GetSession(ctx, db.GetSessionParams{Principal: test.principal, SessionID: "before"})
		require.NoError(t, err)
		assert.Equal(t, test.wantKind, session.Kind, "backfilled kind of %s (%s)", test.principal, test.attestation)
	}

	// A 0.2 replica keeps inserting sessions without a kind during the
	// rolling upgrade, and they get the same kind.
	for _, test := range cases {
		_, err := pool.Exec(ctx, insertPhase2Session, test.principal, "during", test.attestation)
		require.NoError(t, err, "a 0.2-shaped insert after the migration")

		session, err := queries.GetSession(ctx, db.GetSessionParams{Principal: test.principal, SessionID: "during"})
		require.NoError(t, err)
		assert.Equal(t, test.wantKind, session.Kind, "kind of a 0.2 replica's session %s (%s)", test.principal, test.attestation)
	}

	// The kind written by this version is kept, even where the attestation
	// and principal would suggest another.
	written, err := queries.CreateSession(ctx, db.CreateSessionParams{
		Principal: "system:serviceaccount:team:bot", SessionID: "attested-user", Workspace: "coder/alice/w",
		Attestation: "attested", Kind: "user", Groups: []string{},
	})
	require.NoError(t, err)
	assert.Equal(t, "user", written.Kind)

	restarted, err := pool.Exec(ctx, "UPDATE sessions SET ended_at = now() WHERE id = $1", written.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, restarted.RowsAffected())

	session, err := queries.RestartSession(ctx, db.RestartSessionParams{
		ID: written.ID, Workspace: "k8s/team/Deployment/bot", Attestation: "attested", Kind: "service-account",
	})
	require.NoError(t, err)
	assert.Equal(t, "service-account", session.Kind, "a restart writes the restarting identity's kind")
}

func TestScopingConstraints(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)

	session := newSession(t, queries, "alice")

	var scopeID int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO user_scopes (principal) VALUES ('alice') RETURNING id").Scan(&scopeID))

	scope, err := queries.GetUserScope(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, "none", scope.VerifiedDefault, "verified default")
	assert.Equal(t, "none", scope.UnverifiedDefault, "unverified default")

	_, err = pool.Exec(ctx, "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, $2)", scopeID, strings.Repeat("w", 256))
	require.NoError(t, err, "a workspace of exactly 256 characters is allowed")

	for _, test := range []struct {
		name      string
		statement string
		arguments []any
		code      string
	}{
		{"session kind outside the three kinds", "INSERT INTO sessions (principal, session_id, workspace, attestation, kind) VALUES ('alice', 'k', 'w', 'user', 'robot')", nil, "23514"},
		{"verified default outside none and all", "INSERT INTO user_scopes (principal, verified_default) VALUES ('bob', 'some')", nil, "23514"},
		{"unverified default outside none and all", "INSERT INTO user_scopes (principal, unverified_default) VALUES ('bob', 'some')", nil, "23514"},
		{"second scope row for a principal", "INSERT INTO user_scopes (principal) VALUES ('alice')", nil, "23505"},
		{"workspace longer than 256 characters", "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, $2)", []any{scopeID, strings.Repeat("x", 257)}, "23514"},
		{"claimed workspace", "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, 'claimed/default')", []any{scopeID}, "23514"},
		{"second list for a workspace", "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, $2)", []any{scopeID, strings.Repeat("w", 256)}, "23505"},
		{"session addition of a missing session", "INSERT INTO session_additions (session_pk, server, request_id) VALUES (-1, 'github', $1)", []any{rowID(1)}, "23503"},
		{"server request without a session", "INSERT INTO access_requests (id, principal, kind, workspace, verified, server, expires_at) VALUES ($1, 'alice', 'server', 'w', true, 'github', now() + interval '1 hour')", []any{rowID(2)}, "23514"},
		{"change request without a change", "INSERT INTO access_requests (id, principal, kind, workspace, verified, expires_at) VALUES ($1, 'alice', 'change', 'w', true, now() + interval '1 hour')", []any{rowID(3)}, "23514"},
		{"request status outside the four", "INSERT INTO access_requests (id, principal, kind, session_pk, workspace, verified, server, status, expires_at) VALUES ($1, 'alice', 'server', $2, 'w', true, 'github', 'used', now() + interval '1 hour')", []any{rowID(4), session}, "23514"},
		{"decided scope outside session and workspace", "INSERT INTO access_requests (id, principal, kind, session_pk, workspace, verified, server, decided_scope, expires_at) VALUES ($1, 'alice', 'server', $2, 'w', true, 'github', 'forever', now() + interval '1 hour')", []any{rowID(5), session}, "23514"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(), test.statement, test.arguments...)

			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, test.code, pgErr.Code)
		})
	}

	_, err = pool.Exec(ctx, "INSERT INTO access_requests (id, principal, kind, session_pk, workspace, verified, server, expires_at) VALUES ($1, 'alice', 'server', $2, 'w', true, 'github', now() + interval '1 hour')", rowID(6), session)
	require.NoError(t, err, "a server request")
	_, err = pool.Exec(ctx, `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at) VALUES ($1, 'alice', 'change', 'w', true, '{"add":["github"]}', now() + interval '1 hour')`, rowID(7))
	require.NoError(t, err, "a change request")
}

func TestScopingReadQueries(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)

	_, err := queries.GetUserScope(ctx, "alice")
	require.ErrorIs(t, err, pgx.ErrNoRows, "no row before the first scoping write")

	var aliceScope, bobScope int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO user_scopes (principal, verified_default) VALUES ('alice', 'all') RETURNING id").Scan(&aliceScope))
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO user_scopes (principal) VALUES ('bob') RETURNING id").Scan(&bobScope))

	scope, err := queries.GetUserScope(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, aliceScope, scope.ID)
	assert.Equal(t, "all", scope.VerifiedDefault)
	assert.Equal(t, "none", scope.UnverifiedDefault)

	var mailList, emptyList, bobList int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, 'coder/alice/mail') RETURNING id", aliceScope).Scan(&mailList))
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, 'coder/alice/empty') RETURNING id", aliceScope).Scan(&emptyList))
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO workspace_lists (scope_id, workspace) VALUES ($1, 'coder/alice/mail') RETURNING id", bobScope).Scan(&bobList))
	_, err = pool.Exec(ctx, "INSERT INTO workspace_list_servers (list_id, server) VALUES ($1, 'mail'), ($1, 'github'), ($2, 'k8s')", mailList, bobList)
	require.NoError(t, err)

	servers, err := queries.GetWorkspaceListServers(ctx, db.GetWorkspaceListServersParams{Principal: "alice", Workspace: "coder/alice/mail"})
	require.NoError(t, err)
	assert.Equal(t, []db.GetWorkspaceListServersRow{
		{ListID: mailList, Server: pgtype.Text{String: "github", Valid: true}},
		{ListID: mailList, Server: pgtype.Text{String: "mail", Valid: true}},
	}, servers, "only alice's list for that workspace")

	servers, err = queries.GetWorkspaceListServers(ctx, db.GetWorkspaceListServersParams{Principal: "alice", Workspace: "coder/alice/empty"})
	require.NoError(t, err)
	assert.Equal(t, []db.GetWorkspaceListServersRow{{ListID: emptyList}}, servers, "an empty list is one row without a server")

	servers, err = queries.GetWorkspaceListServers(ctx, db.GetWorkspaceListServersParams{Principal: "alice", Workspace: "coder/alice/none"})
	require.NoError(t, err)
	assert.Empty(t, servers, "no list")

	aliceSession := newSession(t, queries, "alice")
	bobSession := newSession(t, queries, "bob")
	_, err = pool.Exec(ctx, "INSERT INTO session_additions (session_pk, server, request_id) VALUES ($1, 'immich', $3), ($1, 'archive', $3), ($2, 'k8s', $3)",
		aliceSession, bobSession, rowID(1))
	require.NoError(t, err)

	additions, err := queries.ListSessionAdditions(ctx, db.ListSessionAdditionsParams{Principal: "alice", SessionID: "s-alice"})
	require.NoError(t, err)
	assert.Equal(t, []string{"archive", "immich"}, additions)

	additions, err = queries.ListSessionAdditions(ctx, db.ListSessionAdditionsParams{Principal: "bob", SessionID: "s-alice"})
	require.NoError(t, err)
	assert.Empty(t, additions, "another principal's session ID never matches")
}
