//go:build integration

package store_test

import (
	"fmt"
	"testing"

	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	upstreamInstanceA = "000000000000000000000000000000a1"
	upstreamInstanceB = "000000000000000000000000000000b1"
)

type upstreamFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	session db.Session
}

func newUpstreamFixture(t *testing.T) upstreamFixture {
	t.Helper()

	_, pool := migratedPool(t)
	queries := db.New(pool)

	session, err := queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: "alice", SessionID: "upstream", Workspace: "claimed/default", Attestation: "user", Kind: "user",
		Groups: []string{},
	})
	require.NoError(t, err)

	return upstreamFixture{pool: pool, queries: queries, session: session}
}

func (f upstreamFixture) instance(t *testing.T, id, namespace string) {
	t.Helper()

	_, err := f.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: "catalog", TemplateName: "github", TemplateUid: "uid-1", Generation: 1,
		Scope: "shared", ScopeKey: "shared-" + id, Namespace: namespace,
	})
	require.NoError(t, err)
}

func (f upstreamFixture) enable(t *testing.T) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(),
		"INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, 'catalog', 'github')", f.session.ID)
	require.NoError(t, err)
}

func (f upstreamFixture) insert(t *testing.T, instanceID, upstreamID string) (int64, error) {
	t.Helper()

	return f.queries.InsertUpstreamSession(t.Context(), db.InsertUpstreamSessionParams{
		InstanceID: instanceID, SessionPk: f.session.ID, UpstreamSessionID: upstreamID,
		InitializeResult: []byte(`{"protocolVersion":"2025-11-25"}`),
	})
}

func TestUpstreamSessionLiveRowIsUniquePerPair(t *testing.T) {
	f := newUpstreamFixture(t)
	f.instance(t, upstreamInstanceA, "tg-a")

	first, err := f.insert(t, upstreamInstanceA, "one")
	require.NoError(t, err)

	_, err = f.insert(t, upstreamInstanceA, "two")
	require.ErrorIs(t, err, pgx.ErrNoRows, "a conflict with a live row returns no id")

	live, err := f.queries.GetLiveUpstreamSession(t.Context(), db.GetLiveUpstreamSessionParams{
		InstanceID: upstreamInstanceA, SessionPk: f.session.ID})
	require.NoError(t, err)
	assert.Equal(t, first, live.ID)
	assert.Equal(t, "one", live.UpstreamSessionID)

	// A plain insert is refused by the index.
	_, err = f.pool.Exec(t.Context(),
		"INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result) VALUES ($1, $2, 'x', '{}')",
		upstreamInstanceA, f.session.ID)

	var pgErr *pgconn.PgError

	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "upstream_sessions_live_idx", pgErr.ConstraintName)

	rows, err := f.queries.EndUpstreamSessionsOfSession(t.Context(), f.session.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	_, err = f.insert(t, upstreamInstanceA, "three")
	require.NoError(t, err, "a second row is accepted once the first ended")
}

func TestUpstreamSessionIDLengthIsChecked(t *testing.T) {
	f := newUpstreamFixture(t)
	f.instance(t, upstreamInstanceA, "tg-a")

	for _, test := range []struct {
		name   string
		length int
		ok     bool
	}{{"empty", 0, false}, {"one", 1, true}, {"limit", 1024, true}, {"over", 1025, false}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.pool.Exec(t.Context(),
				"INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result) VALUES ($1, $2, repeat('a', $3), '{}')",
				upstreamInstanceA, f.session.ID, test.length)
			if test.ok {
				require.NoError(t, err)

				_, err = f.queries.EndUpstreamSessionsOfSession(t.Context(), f.session.ID)
				require.NoError(t, err)

				return
			}

			var pgErr *pgconn.PgError

			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, "23514", pgErr.Code)
		})
	}
}

func TestUpstreamSessionsAreRemovedWithInstanceAndSession(t *testing.T) {
	f := newUpstreamFixture(t)
	f.instance(t, upstreamInstanceA, "tg-a")
	f.instance(t, upstreamInstanceB, "tg-b")

	_, err := f.insert(t, upstreamInstanceA, "one")
	require.NoError(t, err)
	_, err = f.insert(t, upstreamInstanceB, "two")
	require.NoError(t, err)

	count := func() int {
		var n int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM upstream_sessions").Scan(&n))

		return n
	}

	_, err = f.pool.Exec(t.Context(), "DELETE FROM instances WHERE id = $1", upstreamInstanceA)
	require.NoError(t, err)
	assert.Equal(t, 1, count())

	_, err = f.pool.Exec(t.Context(), "DELETE FROM sessions WHERE id = $1", f.session.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, count())
}

func TestUpstreamClaimQueries(t *testing.T) {
	f := newUpstreamFixture(t)
	f.instance(t, upstreamInstanceA, "tg-a")
	ctx := t.Context()

	_, err := f.queries.LockLiveSessionForKeyShare(ctx, f.session.ID)
	require.NoError(t, err)

	enabled, err := f.queries.SessionHasServerForInstanceTemplate(ctx, db.SessionHasServerForInstanceTemplateParams{
		SessionPk: f.session.ID, InstanceID: upstreamInstanceA})
	require.NoError(t, err)
	assert.False(t, enabled, "no session_servers row")

	f.enable(t)

	enabled, err = f.queries.SessionHasServerForInstanceTemplate(ctx, db.SessionHasServerForInstanceTemplateParams{
		SessionPk: f.session.ID, InstanceID: upstreamInstanceA})
	require.NoError(t, err)
	assert.True(t, enabled)

	_, err = f.pool.Exec(ctx, "UPDATE sessions SET ended_at = now() WHERE id = $1", f.session.ID)
	require.NoError(t, err)

	_, err = f.queries.LockLiveSessionForKeyShare(ctx, f.session.ID)
	require.ErrorIs(t, err, pgx.ErrNoRows, "an ended session can't be claimed")
}

func TestDeleteUpstreamSessionIfUnchangedMatchesOnlyTheOldValue(t *testing.T) {
	f := newUpstreamFixture(t)
	f.instance(t, upstreamInstanceA, "tg-a")

	id, err := f.insert(t, upstreamInstanceA, "old")
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), "UPDATE upstream_sessions SET upstream_session_id = 'new' WHERE id = $1", id)
	require.NoError(t, err)

	rows, err := f.queries.DeleteUpstreamSessionIfUnchanged(t.Context(), db.DeleteUpstreamSessionIfUnchangedParams{ID: id, UpstreamSessionID: "old"})
	require.NoError(t, err)
	assert.Zero(t, rows)

	rows, err = f.queries.DeleteUpstreamSessionIfUnchanged(t.Context(), db.DeleteUpstreamSessionIfUnchangedParams{ID: id, UpstreamSessionID: "new"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)
}

func TestEndUpstreamSessionsOfTemplateAndSession(t *testing.T) {
	f := newUpstreamFixture(t)
	f.instance(t, upstreamInstanceA, "tg-a")

	_, err := f.pool.Exec(t.Context(), `INSERT INTO instances (id, template_ns, template_name, template_uid, generation, scope, scope_key, namespace)
		VALUES ($1, 'catalog', 'other', 'uid-2', 1, 'shared', 'shared-other', 'tg-b')`, upstreamInstanceB)
	require.NoError(t, err)

	_, err = f.insert(t, upstreamInstanceA, "one")
	require.NoError(t, err)
	_, err = f.insert(t, upstreamInstanceB, "two")
	require.NoError(t, err)

	rows, err := f.queries.EndUpstreamSessionsOfTemplate(t.Context(), db.EndUpstreamSessionsOfTemplateParams{
		SessionPk: f.session.ID, TemplateNs: "catalog", TemplateName: "github"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows, "only the template's rows end")

	live, err := f.queries.ListLiveUpstreamSessionsOfSession(t.Context(), f.session.ID)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, upstreamInstanceB, live[0].InstanceID)
	assert.Equal(t, "two", live[0].UpstreamSessionID)

	rows, err = f.queries.EndUpstreamSessionsOfSession(t.Context(), f.session.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	live, err = f.queries.ListLiveUpstreamSessionsOfSession(t.Context(), f.session.ID)
	require.NoError(t, err)
	assert.Empty(t, live)
}

func TestEndInvalidUpstreamSessionsEndsOnlyInvalidPairs(t *testing.T) {
	f := newUpstreamFixture(t)
	ctx := t.Context()

	f.instance(t, upstreamInstanceA, "tg-a")
	f.enable(t)

	_, err := f.insert(t, upstreamInstanceA, "valid")
	require.NoError(t, err)

	ended := func() int {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM upstream_sessions WHERE ended_at IS NOT NULL").Scan(&n))

		return n
	}

	rows, err := f.queries.EndInvalidUpstreamSessions(ctx)
	require.NoError(t, err)
	assert.Zero(t, rows, "a valid pair stays")

	for _, test := range []struct {
		name   string
		break_ string
	}{
		{"ended session", "UPDATE sessions SET ended_at = now()"},
		{"missing session_servers row", "DELETE FROM session_servers"},
		{"instance not live", "UPDATE instances SET status = 'stopping'"},
		{"instance stopped by desire", "UPDATE instances SET desired = 'stopped'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, "DELETE FROM upstream_sessions")
			require.NoError(t, err)
			_, err = f.pool.Exec(ctx, "UPDATE sessions SET ended_at = NULL")
			require.NoError(t, err)
			_, err = f.pool.Exec(ctx, "UPDATE instances SET status = 'pending', desired = 'running'")
			require.NoError(t, err)
			_, err = f.pool.Exec(ctx, "INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, 'catalog', 'github') ON CONFLICT DO NOTHING", f.session.ID)
			require.NoError(t, err)
			_, err = f.insert(t, upstreamInstanceA, "valid")
			require.NoError(t, err)

			_, err = f.pool.Exec(ctx, test.break_)
			require.NoError(t, err)

			rows, err := f.queries.EndInvalidUpstreamSessions(ctx)
			require.NoError(t, err)
			assert.EqualValues(t, 1, rows)
			assert.Equal(t, 1, ended())
		})
	}
}

func TestListEndedUpstreamSessionsReadsOldestFirstUpToALimit(t *testing.T) {
	f := newUpstreamFixture(t)
	ctx := t.Context()

	f.instance(t, upstreamInstanceA, "tg-a")
	f.instance(t, upstreamInstanceB, "")

	// 300 ended rows on the in-cluster instance, one ended long ago on the
	// remote instance. The sessions table gives each row its own pair.
	_, err := f.pool.Exec(ctx, `INSERT INTO sessions (principal, session_id, workspace, attestation)
		SELECT 'bob', 's' || n, 'w', 'user' FROM generate_series(1, 300) AS n`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result, ended_at)
		SELECT $1, id, 'u' || id, '{}', now() - make_interval(secs => 1000 - id) FROM sessions WHERE principal = 'bob'`, upstreamInstanceA)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result, ended_at)
		VALUES ($1, $2, 'oldest', '{}', now() - interval '1 day')`, upstreamInstanceB, f.session.ID)
	require.NoError(t, err)

	// A live row is not listed.
	_, err = f.insert(t, upstreamInstanceA, "live")
	require.NoError(t, err)

	rows, err := f.queries.ListEndedUpstreamSessions(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 256)
	assert.Equal(t, "oldest", rows[0].UpstreamSessionID)
	assert.True(t, rows[0].InstanceRemote)
	assert.True(t, rows[0].InstanceLive)
	assert.False(t, rows[1].InstanceRemote)

	var firstBob int64

	require.NoError(t, f.pool.QueryRow(ctx, "SELECT min(id) FROM sessions WHERE principal = 'bob'").Scan(&firstBob))

	for i := 1; i < len(rows); i++ {
		assert.Equal(t, fmt.Sprintf("u%d", firstBob+int64(i-1)), rows[i].UpstreamSessionID, "oldest first")
	}

	_, err = f.pool.Exec(ctx, "UPDATE instances SET status = 'stopped', desired = 'stopped' WHERE id = $1", upstreamInstanceA)
	require.NoError(t, err)

	rows, err = f.queries.ListEndedUpstreamSessions(ctx)
	require.NoError(t, err)
	assert.False(t, rows[1].InstanceLive)

	require.NoError(t, f.queries.DeleteUpstreamSession(ctx, rows[0].ID))

	rows, err = f.queries.ListEndedUpstreamSessions(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, "oldest", rows[0].UpstreamSessionID)
}

// upstreamSessionsVersion is the migration that adds upstream_sessions.
const upstreamSessionsVersion = 15

func TestUpstreamSessionsMigrationAppliesToThePreviousVersion(t *testing.T) {
	pool := openPool(t, testutil.StartPostgresEmpty(t).ConnString())
	ctx := t.Context()

	sqlDB := stdlib.OpenDBFromPool(pool)

	t.Cleanup(func() { _ = sqlDB.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, upstreamSessionsVersion-1)
	require.NoError(t, err)

	var exists bool

	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('public.upstream_sessions') IS NOT NULL").Scan(&exists))
	require.False(t, exists)

	require.NoError(t, store.Migrate(ctx, pool))
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('public.upstream_sessions') IS NOT NULL").Scan(&exists))
	assert.True(t, exists)
}
