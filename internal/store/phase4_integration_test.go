//go:build integration

package store_test

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// phase3Version is the last migration a 0.3 gateway knows.
const phase3Version = 6

func TestPhase4MigrationKeepsPhase3Rows(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	ctx := t.Context()

	sqlDB := stdlib.OpenDBFromPool(pool)

	t.Cleanup(func() { _ = sqlDB.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, phase3Version)
	require.NoError(t, err)

	const insertRequest = `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
		VALUES ($1, 'alice', 'change', 'w', true, '{}', now() + interval '1 hour')`

	_, err = pool.Exec(ctx, insertRequest, "00000000000000000000000000000001")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO user_scopes (principal, unverified_default) VALUES ('alice', 'all')")
	require.NoError(t, err)

	require.NoError(t, store.Migrate(ctx, pool))

	var kind string

	require.NoError(t, pool.QueryRow(ctx, "SELECT kind FROM access_requests").Scan(&kind))
	assert.Equal(t, "change", kind)

	var unverifiedList bool

	require.NoError(t, pool.QueryRow(ctx, "SELECT unverified_list FROM user_scopes").Scan(&unverifiedList))
	assert.False(t, unverifiedList)

	// A 0.3-shaped insert still passes both checks.
	_, err = pool.Exec(ctx, insertRequest, "00000000000000000000000000000002")
	require.NoError(t, err)

	insertScope := func(id, change string) error {
		_, err := pool.Exec(ctx, `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
			VALUES ($1, 'alice', $2, 'w', true, $3, now() + interval '1 hour')`, id, "scope", nullIfEmpty(change))

		return err
	}

	require.NoError(t, insertScope("00000000000000000000000000000003", `{"a":1}`))
	require.Error(t, insertScope("00000000000000000000000000000004", ""), "a scope request needs its change")

	_, err = pool.Exec(ctx, `INSERT INTO access_requests (id, principal, kind, workspace, verified, expires_at)
		VALUES ('00000000000000000000000000000005', 'alice', 'bogus', 'w', true, now() + interval '1 hour')`)
	require.Error(t, err, "unknown kinds are still refused")

	_, err = pool.Exec(ctx, `INSERT INTO access_requests (id, principal, kind, workspace, verified, expires_at)
		VALUES ('00000000000000000000000000000006', 'alice', 'server', 'w', true, now() + interval '1 hour')`)
	require.Error(t, err, "a server request still needs its session and server")

	_, err = pool.Exec(ctx, "UPDATE user_scopes SET unverified_default = 'list'")
	require.Error(t, err, "unverified_default gains no value")
}

func TestPhase4Tables(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)

	session := newSession(t, queries, "alice")

	var scopeID int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO user_scopes (principal) VALUES ('alice') RETURNING id").Scan(&scopeID))

	// Scope entries: exactly one of workspace and session, unique per key.
	workspace := "coder/alice/dev"
	entry := db.InsertScopeEntryParams{ScopeID: scopeID, Workspace: pgtype.Text{String: workspace, Valid: true}, Server: "github", Tool: "", Effect: "allow"}
	row, err := queries.InsertScopeEntry(ctx, entry)
	require.NoError(t, err)
	assert.NotZero(t, row.ID)

	_, err = queries.InsertScopeEntry(ctx, entry)
	require.Error(t, err, "the same entry is not inserted twice (no row returned)")

	entry.Effect = "deny"
	_, err = queries.InsertScopeEntry(ctx, entry)
	require.NoError(t, err, "allow and deny are different keys")

	sessionPK := session
	_, err = queries.InsertScopeEntry(ctx, db.InsertScopeEntryParams{
		ScopeID: scopeID, Workspace: pgtype.Text{String: workspace, Valid: true}, SessionPk: pgtype.Int8{Int64: sessionPK, Valid: true}, Server: "github", Effect: "allow",
	})
	require.Error(t, err, "both workspace and session set")

	_, err = queries.InsertScopeEntry(ctx, db.InsertScopeEntryParams{ScopeID: scopeID, Server: "github", Effect: "allow"})
	require.Error(t, err, "neither workspace nor session set")

	_, err = queries.InsertScopeEntry(ctx, db.InsertScopeEntryParams{
		ScopeID: scopeID, SessionPk: pgtype.Int8{Int64: sessionPK, Valid: true}, Server: "github", Tool: "search", Effect: "maybe",
	})
	require.Error(t, err, "unknown effect")

	_, err = queries.InsertScopeEntry(ctx, db.InsertScopeEntryParams{
		ScopeID: scopeID, SessionPk: pgtype.Int8{Int64: sessionPK, Valid: true}, Server: "github", Tool: "search", Effect: "allow",
	})
	require.NoError(t, err)

	count, err := queries.CountAllowScopeEntries(ctx, scopeID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, count)

	require.NoError(t, queries.DeleteSessionScopeEntries(ctx, db.DeleteSessionScopeEntriesParams{SessionPk: pgtype.Int8{Int64: sessionPK, Valid: true}, Effect: "allow"}))

	entries, err := queries.ListScopeEntries(ctx, scopeID)
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	// Tool catalog and search rows.
	require.NoError(t, queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
		TemplateUid: "uid-1", Generation: 1, TemplateNs: "ns", TemplateName: "gh", Tools: []byte(`[]`), InstanceID: "i",
	}))
	require.NoError(t, queries.UpsertToolCatalog(ctx, db.UpsertToolCatalogParams{
		TemplateUid: "uid-1", Generation: 2, TemplateNs: "ns", TemplateName: "gh", Tools: []byte(`[{"name":"x"}]`), InstanceID: "i",
	}))
	require.NoError(t, queries.DeleteToolCatalogBefore(ctx, db.DeleteToolCatalogBeforeParams{TemplateUid: "uid-1", Generation: 2}))

	catalog, err := queries.ListToolCatalogForTemplate(ctx, "uid-1")
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	assert.EqualValues(t, 2, catalog[0].Generation)

	require.NoError(t, queries.InsertToolSearch(ctx, db.InsertToolSearchParams{
		TemplateUid: "uid-1", TemplateNs: "ns", TemplateName: "gh", SourceGeneration: 2,
		UpstreamName: "create_issue", ExposedName: "gh__create_issue", NamePart: "create issue", DescriptionPart: "create issue in a repository",
	}))

	var matches int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM tool_search WHERE document @@ plainto_tsquery('english', 'repository')").Scan(&matches))
	assert.Equal(t, 1, matches)

	// Found tools and the unverified list.
	require.NoError(t, queries.AddSessionFoundTool(ctx, db.AddSessionFoundToolParams{
		SessionPk: session, TemplateNs: "ns", TemplateName: "gh", UpstreamName: "create_issue",
	}))
	require.NoError(t, queries.AddSessionFoundTool(ctx, db.AddSessionFoundToolParams{
		SessionPk: session, TemplateNs: "ns", TemplateName: "gh", UpstreamName: "create_issue",
	}))

	found, err := queries.CountSessionFoundTools(ctx, session)
	require.NoError(t, err)
	assert.EqualValues(t, 1, found)

	added, err := queries.AddUnverifiedServers(ctx, db.AddUnverifiedServersParams{ScopeID: scopeID, Servers: []string{"a", "b"}})
	require.NoError(t, err)
	assert.Len(t, added, 2)

	require.NoError(t, queries.DeleteUnverifiedServers(ctx, db.DeleteUnverifiedServersParams{ScopeID: scopeID, Servers: []string{"a"}}))

	servers, err := queries.ListUnverifiedServers(ctx, scopeID)
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, servers)
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}
