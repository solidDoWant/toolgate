//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func rowID(number int) string { return fmt.Sprintf("%032x", number) }

func interval(duration time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: duration.Microseconds(), Valid: true}
}

func hash32(value string) []byte {
	sum := sha256.Sum256([]byte(value))

	return sum[:]
}

func grantParams(number int, principal string) db.InsertGrantWithinLimitParams {
	return db.InsertGrantWithinLimitParams{
		ID: rowID(number), Principal: principal, TemplateNs: "catalog", TemplateName: "github",
		Lifetime: interval(time.Hour),
	}
}

func blockParams(number int) db.InsertTemporaryBlockWithinLimitParams {
	return db.InsertTemporaryBlockWithinLimitParams{
		ID: rowID(number), Match: []byte(`{"tools":["delete_*"]}`), Reason: "incident", CreatedBy: "admin",
		Lifetime: interval(time.Hour),
	}
}

func approvalParams(number int, principal string, sessionPK int64) db.InsertApprovalWithinLimitParams {
	return db.InsertApprovalWithinLimitParams{
		ID: rowID(number), Principal: principal, SessionPk: sessionPK, Workspace: "repo",
		TemplateNs: "catalog", TemplateName: "github", Tool: "merge", ArgumentsSha256: hash32(rowID(number)),
		ApprovalTimeout: interval(10 * time.Minute),
	}
}

func newSession(t *testing.T, queries *db.Queries, principal string) int64 {
	t.Helper()

	session, err := queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: principal, SessionID: "s-" + principal, Workspace: "repo", Attestation: "user", Kind: "user", Groups: []string{},
	})
	require.NoError(t, err)

	return session.ID
}

func TestPhase2MigrationSchema(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()

	for _, table := range []string{"grants", "temporary_blocks", "approvals", "ui_sessions", "ui_logins"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists))
		assert.True(t, exists, "table %s", table)
	}

	for column, dataType := range map[string]string{
		"restart_requested_at": "timestamp with time zone",
		"restart_requested_by": "text",
		"resolved_expires_at":  "timestamp with time zone",
	} {
		var actual, nullable string
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT data_type, is_nullable FROM information_schema.columns WHERE table_name = 'instances' AND column_name = $1",
			column).Scan(&actual, &nullable), column)
		assert.Equal(t, dataType, actual, column)
		assert.Equal(t, "YES", nullable, column)
	}

	var definition string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT indexdef FROM pg_indexes WHERE tablename = 'grants' AND indexname = 'grants_principal_live_idx'").Scan(&definition))
	assert.Contains(t, definition, "(principal) WHERE (revoked_at IS NULL)")

	session := newSession(t, db.New(pool), "alice")

	for _, test := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"grant longer than 7 days", "INSERT INTO grants (id, principal, template_ns, template_name, expires_at) VALUES ($1, 'alice', 'catalog', 'github', now() + interval '7 days 1 second')", []any{rowID(1)}},
		{"grant expiring at creation", "INSERT INTO grants (id, principal, template_ns, template_name, expires_at) VALUES ($1, 'alice', 'catalog', 'github', now())", []any{rowID(2)}},
		{"grant with a malformed ID", "INSERT INTO grants (id, principal, template_ns, template_name, expires_at) VALUES ('ABC', 'alice', 'catalog', 'github', now() + interval '1 hour')", nil},
		{"block longer than 30 days", "INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at) VALUES ($1, '{}', 'r', 'admin', now() + interval '30 days 1 second')", []any{rowID(3)}},
		{"block without a reason", "INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at) VALUES ($1, '{}', '', 'admin', now() + interval '1 hour')", []any{rowID(4)}},
		{"block with a 1025-character reason", "INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at) VALUES ($1, '{}', repeat('r', 1025), 'admin', now() + interval '1 hour')", []any{rowID(5)}},
		{"approval with an unknown status", "INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, status, expires_at) VALUES ($1, 'alice', $2, 'repo', 'catalog', 'github', 'merge', $3, 'done', now() + interval '1 hour')", []any{rowID(6), session, hash32("x")}},
		{"approval with a short hash", "INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, expires_at) VALUES ($1, 'alice', $2, 'repo', 'catalog', 'github', 'merge', '\\x00', now() + interval '1 hour')", []any{rowID(7), session}},
		{"ui session longer than 8 hours", "INSERT INTO ui_sessions (id_hash, principal, csrf_token, expires_at) VALUES ($1, 'alice', $1, now() + interval '8 hours 1 second')", []any{hash32("a")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(), test.statement, test.arguments...)

			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, "23514", pgErr.Code, "a CHECK constraint refuses it")
		})
	}

	_, err := pool.Exec(ctx, "INSERT INTO grants (id, principal, template_ns, template_name, expires_at) VALUES ($1, 'alice', 'catalog', 'github', now() + interval '7 days')", rowID(8))
	require.NoError(t, err, "exactly 7 days is allowed")
	_, err = pool.Exec(ctx, "INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at) VALUES ($1, '{}', repeat('r', 1024), 'admin', now() + interval '30 days')", rowID(9))
	require.NoError(t, err, "exactly 30 days and 1024 characters are allowed")
}

func TestPhase2MigrationKeepsPhase1Rows(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	ctx := t.Context()

	sqlDB := stdlib.OpenDBFromPool(pool)

	t.Cleanup(func() { _ = sqlDB.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 2)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO instances (id, template_ns, template_name, template_uid, generation, scope, scope_key, namespace)
		VALUES ($1, 'catalog', 'github', 'uid', 1, 'shared', 'shared', 'tg')`, rowID(1))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO sessions (principal, session_id, workspace, attestation) VALUES ('alice', 's1', 'repo', 'user')`)
	require.NoError(t, err)

	snapshot := func() string {
		var rows string
		// sessions.kind, last_search_id and tool_search came with later migrations.

		return rows
	}
	before := snapshot()

	require.NoError(t, store.Migrate(ctx, pool))

	assert.Equal(t, before, snapshot(), "existing rows are unchanged")

	var (
		restartAt, resolved pgtype.Timestamptz
		restartBy           pgtype.Text
	)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT restart_requested_at, restart_requested_by, resolved_expires_at FROM instances WHERE id = $1", rowID(1)).
		Scan(&restartAt, &restartBy, &resolved))
	assert.False(t, restartAt.Valid)
	assert.False(t, restartBy.Valid)
	assert.False(t, resolved.Valid)
}

// limitCase is one row limit: fill inserts limit-1 rows, lockAndInsert
// takes the lock and inserts one row in tx, and insert runs the store's
// insert.
type limitCase struct {
	name          string
	limit         int
	fill          func(t *testing.T, pool *pgxpool.Pool, count int)
	lockAndInsert func(ctx context.Context, tx pgx.Tx, number int) error
	insert        func(ctx context.Context, pool *pgxpool.Pool, number int) error
}

func limitCases(t *testing.T, pool *pgxpool.Pool) []limitCase {
	t.Helper()

	session := newSession(t, db.New(pool), "alice")

	return []limitCase{
		{
			name:  "grants per principal",
			limit: store.MaxLiveGrantsPerPrincipal,
			fill: func(t *testing.T, pool *pgxpool.Pool, count int) {
				t.Helper()
				// Rows that aren't live don't count: one revoked, one expired, one of another principal.
				_, err := pool.Exec(t.Context(), `INSERT INTO grants (id, principal, template_ns, template_name, created_at, expires_at)
					VALUES ($1, 'alice', 'c', 'g', now() - interval '2 hours', now() - interval '1 hour')`, rowID(900001))
				require.NoError(t, err)
				_, err = pool.Exec(t.Context(), `INSERT INTO grants (id, principal, template_ns, template_name, expires_at, revoked_at)
					VALUES ($1, 'alice', 'c', 'g', now() + interval '1 hour', now())`, rowID(900002))
				require.NoError(t, err)
				_, err = store.InsertGrant(t.Context(), pool, grantParams(900003, "bob"))
				require.NoError(t, err)

				for number := range count {
					_, err := store.InsertGrant(t.Context(), pool, grantParams(number, "alice"))
					require.NoError(t, err, "grant %d", number)
				}
			},
			lockAndInsert: func(ctx context.Context, tx pgx.Tx, number int) error {
				queries := db.New(tx)
				if err := queries.LockPrincipalGrants(ctx, "alice"); err != nil {
					return err
				}

				params := grantParams(number, "alice")
				params.LiveLimit = store.MaxLiveGrantsPerPrincipal
				_, err := queries.InsertGrantWithinLimit(ctx, params)

				return err
			},
			insert: func(ctx context.Context, pool *pgxpool.Pool, number int) error {
				_, err := store.InsertGrant(ctx, pool, grantParams(number, "alice"))

				return err
			},
		},
		{
			name:  "temporary blocks",
			limit: store.MaxLiveTemporaryBlocks,
			fill: func(t *testing.T, pool *pgxpool.Pool, count int) {
				t.Helper()
				// An expired block doesn't count.
				_, err := pool.Exec(t.Context(), `INSERT INTO temporary_blocks (id, match, reason, created_by, created_at, expires_at)
					VALUES ($1, '{}', 'r', 'admin', now() - interval '2 hours', now() - interval '1 hour')`, rowID(900001))
				require.NoError(t, err)
				_, err = pool.Exec(t.Context(), `INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at)
					SELECT lpad(to_hex(number), 32, '0'), '{}', 'r', 'admin', now() + interval '1 hour' FROM generate_series(0, $1 - 1) AS number`, count)
				require.NoError(t, err)
			},
			lockAndInsert: func(ctx context.Context, tx pgx.Tx, number int) error {
				queries := db.New(tx)
				if err := queries.LockTemporaryBlockInsert(ctx); err != nil {
					return err
				}

				params := blockParams(number)
				params.LiveLimit = store.MaxLiveTemporaryBlocks
				_, err := queries.InsertTemporaryBlockWithinLimit(ctx, params)

				return err
			},
			insert: func(ctx context.Context, pool *pgxpool.Pool, number int) error {
				_, err := store.InsertTemporaryBlock(ctx, pool, blockParams(number))

				return err
			},
		},
		{
			name:  "pending approvals per principal",
			limit: store.MaxPendingApprovalsPerPrincipal,
			fill: func(t *testing.T, pool *pgxpool.Pool, count int) {
				t.Helper()
				// A denied approval doesn't count.
				denied, err := store.InsertApproval(t.Context(), pool, approvalParams(900001, "alice", session))
				require.NoError(t, err)
				_, err = db.New(pool).DenyApproval(t.Context(), db.DenyApprovalParams{ID: denied.ID})
				require.NoError(t, err)

				for number := range count {
					_, err := store.InsertApproval(t.Context(), pool, approvalParams(number, "alice", session))
					require.NoError(t, err, "approval %d", number)
				}
			},
			lockAndInsert: func(ctx context.Context, tx pgx.Tx, number int) error {
				queries := db.New(tx)
				if err := queries.LockPrincipalApprovals(ctx, "alice"); err != nil {
					return err
				}

				params := approvalParams(number, "alice", session)
				params.PendingLimit = store.MaxPendingApprovalsPerPrincipal
				_, err := queries.InsertApprovalWithinLimit(ctx, params)

				return err
			},
			insert: func(ctx context.Context, pool *pgxpool.Pool, number int) error {
				_, err := store.InsertApproval(ctx, pool, approvalParams(number, "alice", session))

				return err
			},
		},
	}
}

func TestRowLimitsHoldUnderConcurrentInserts(t *testing.T) {
	for index := range 3 {
		_, pool := migratedPool(t)
		test := limitCases(t, pool)[index]

		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			test.fill(t, pool, test.limit-1)

			// The first insert holds the lock and its row uncommitted while
			// the second waits for the lock.
			first, err := pool.Begin(ctx)
			require.NoError(t, err)

			defer func() { _ = first.Rollback(ctx) }()

			require.NoError(t, test.lockAndInsert(ctx, first, 800001), "row number %d is within the limit", test.limit)

			second := make(chan error, 1)
			go func() { second <- test.insert(ctx, pool, 800002) }()

			require.Eventually(t, func() bool {
				var waiting int

				err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted").Scan(&waiting)

				return err == nil && waiting == 1
			}, 10*time.Second, 10*time.Millisecond, "the second insert waits for the lock")

			require.NoError(t, first.Commit(ctx))
			require.ErrorIs(t, <-second, store.ErrLimitReached, "row number %d is refused", test.limit+1)

			require.ErrorIs(t, test.insert(ctx, pool, 800003), store.ErrLimitReached)
		})
	}
}

func TestPhase2CompareAndSetWrites(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)
	session := newSession(t, queries, "alice")
	by := pgtype.Text{String: "alice", Valid: true}

	t.Run("grant revoke", func(t *testing.T) {
		grant, err := store.InsertGrant(ctx, pool, grantParams(1, "alice"))
		require.NoError(t, err)

		revoked, err := queries.RevokeGrant(ctx, db.RevokeGrantParams{ID: grant.ID, RevokedBy: by})
		require.NoError(t, err)
		assert.True(t, revoked.RevokedAt.Valid)

		_, err = queries.RevokeGrant(ctx, db.RevokeGrantParams{ID: grant.ID, RevokedBy: pgtype.Text{String: "admin", Valid: true}})
		require.ErrorIs(t, err, pgx.ErrNoRows, "a grant is revoked once")

		stored, err := queries.GetGrant(ctx, grant.ID)
		require.NoError(t, err)
		assert.Equal(t, "alice", stored.RevokedBy.String)
	})

	t.Run("approval decisions", func(t *testing.T) {
		approval, err := store.InsertApproval(ctx, pool, approvalParams(10, "alice", session))
		require.NoError(t, err)
		assert.Equal(t, "pending", approval.Status)

		_, err = queries.UseApproval(ctx, approval.ID)
		require.ErrorIs(t, err, pgx.ErrNoRows, "a pending approval isn't usable")

		approved, err := queries.ApproveApproval(ctx, db.ApproveApprovalParams{ID: approval.ID, DecidedBy: by, ApprovalTimeout: interval(time.Hour)})
		require.NoError(t, err)
		assert.Equal(t, "approved", approved.Status)
		assert.True(t, approved.ExpiresAt.Time.After(approval.ExpiresAt.Time), "approving restarts the window")

		_, err = queries.DenyApproval(ctx, db.DenyApprovalParams{ID: approval.ID, DecidedBy: by})
		require.ErrorIs(t, err, pgx.ErrNoRows, "a decided approval can't be denied")
		_, err = queries.ExpireApproval(ctx, approval.ID)
		require.ErrorIs(t, err, pgx.ErrNoRows, "a decided approval can't expire")

		used, err := queries.UseApproval(ctx, approval.ID)
		require.NoError(t, err)
		assert.True(t, used.UsedAt.Valid)

		_, err = queries.UseApproval(ctx, approval.ID)
		require.ErrorIs(t, err, pgx.ErrNoRows, "an approval lets one call through")
	})

	t.Run("approve and deny refuse an expired approval", func(t *testing.T) {
		approval, err := store.InsertApproval(ctx, pool, approvalParams(20, "alice", session))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, "UPDATE approvals SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1", approval.ID)
		require.NoError(t, err)

		_, err = queries.ApproveApproval(ctx, db.ApproveApprovalParams{ID: approval.ID, DecidedBy: by, ApprovalTimeout: interval(time.Hour)})
		require.ErrorIs(t, err, pgx.ErrNoRows)
		_, err = queries.DenyApproval(ctx, db.DenyApprovalParams{ID: approval.ID, DecidedBy: by})
		require.ErrorIs(t, err, pgx.ErrNoRows)

		expired, err := queries.ExpireApproval(ctx, approval.ID)
		require.NoError(t, err)
		assert.Equal(t, "expired", expired.Status)

		_, err = queries.ExpireApproval(ctx, approval.ID)
		require.ErrorIs(t, err, pgx.ErrNoRows, "an approval expires once")
	})

	t.Run("expired approved approval isn't usable", func(t *testing.T) {
		approval, err := store.InsertApproval(ctx, pool, approvalParams(30, "alice", session))
		require.NoError(t, err)
		_, err = queries.ApproveApproval(ctx, db.ApproveApprovalParams{ID: approval.ID, DecidedBy: by, ApprovalTimeout: interval(time.Hour)})
		require.NoError(t, err)
		_, err = pool.Exec(ctx, "UPDATE approvals SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1", approval.ID)
		require.NoError(t, err)

		_, err = queries.UseApproval(ctx, approval.ID)
		require.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("ui login is taken once", func(t *testing.T) {
		state := hash32("state")
		require.NoError(t, queries.InsertUILogin(ctx, db.InsertUILoginParams{StateHash: state, Nonce: "n", PkceVerifier: "v", ReturnTo: "/"}))

		login, err := queries.TakeUILogin(ctx, state)
		require.NoError(t, err)
		assert.Equal(t, "n", login.Nonce)

		_, err = queries.TakeUILogin(ctx, state)
		require.ErrorIs(t, err, pgx.ErrNoRows, "each state works once")

		stale := hash32("stale")
		require.NoError(t, queries.InsertUILogin(ctx, db.InsertUILoginParams{StateHash: stale, Nonce: "n", PkceVerifier: "v"}))
		_, err = pool.Exec(ctx, "UPDATE ui_logins SET created_at = now() - interval '10 minutes 1 second' WHERE state_hash = $1", stale)
		require.NoError(t, err)

		_, err = queries.TakeUILogin(ctx, stale)
		require.ErrorIs(t, err, pgx.ErrNoRows, "a sign-in older than 10 minutes doesn't work")
	})
}

func TestDeleteStaleRows(t *testing.T) {
	_, pool := migratedPool(t)
	ctx := t.Context()
	queries := db.New(pool)
	session := newSession(t, queries, "alice")

	kept := map[string][]string{}
	exec := func(table, name, statement string, arguments ...any) {
		t.Helper()

		if strings.HasPrefix(name, "keep-") {
			kept[table] = append(kept[table], name)
		}

		_, err := pool.Exec(ctx, statement, arguments...)
		require.NoError(t, err, statement)
	}

	// A row is named keep-* when it must survive and drop-* when it must go.
	// "Just inside" rows stopped being live 7 days minus 1 minute ago.
	grant := func(name, created, expires, revoked string) {
		exec("grants", name, fmt.Sprintf(`INSERT INTO grants (id, principal, template_ns, template_name, tool, created_at, expires_at, revoked_at)
			VALUES (md5($1), 'alice', 'c', 'g', $1, now() - interval '%s', now() - interval '%s', %s)`, created, expires, revoked), name)
	}
	grant("keep-live", "1 hour", "-1 hour", "NULL")
	grant("keep-expired-recently", "7 days", "6 days 23 hours 59 minutes", "NULL")
	grant("drop-expired", "8 days", "7 days 1 minute", "NULL")
	grant("keep-revoked-recently", "7 days", "1 day", "now() - interval '6 days 23 hours 59 minutes'")
	grant("drop-revoked", "8 days", "1 day", "now() - interval '7 days 1 minute'")

	block := func(name, created, expires string) {
		exec("temporary_blocks", name, fmt.Sprintf(`INSERT INTO temporary_blocks (id, match, reason, created_by, created_at, expires_at)
			VALUES (md5($1), '{}', $1, 'admin', now() - interval '%s', now() - interval '%s')`, created, expires), name)
	}
	block("keep-live", "1 hour", "-1 hour")
	block("keep-expired-recently", "8 days", "6 days 23 hours 59 minutes")
	block("drop-expired", "8 days", "7 days 1 minute")

	approval := func(name, status, created, expires, decided, used string) {
		exec("approvals", name, fmt.Sprintf(`INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, status, created_at, expires_at, decided_at, used_at)
			VALUES (md5($1), 'alice', $2, 'repo', 'c', 'g', $1, sha256($1::bytea), '%s', now() - interval '%s', now() - interval '%s', %s, %s)`,
			status, created, expires, decided, used), name, session)
	}
	approval("keep-pending", "pending", "1 minute", "-10 minutes", "NULL", "NULL")
	approval("keep-approved-unused", "approved", "1 hour", "-1 hour", "now()", "NULL")
	approval("keep-expired-recently", "expired", "7 days 1 hour", "6 days 23 hours 59 minutes", "NULL", "NULL")
	approval("drop-expired", "expired", "8 days", "7 days 1 minute", "NULL", "NULL")
	approval("keep-denied-recently", "denied", "7 days 1 hour", "1 day", "now() - interval '6 days 23 hours 59 minutes'", "NULL")
	approval("drop-denied", "denied", "8 days", "1 day", "now() - interval '7 days 1 minute'", "NULL")
	approval("keep-used-recently", "approved", "7 days 1 hour", "1 day", "now() - interval '7 days'", "now() - interval '6 days 23 hours 59 minutes'")
	approval("drop-used", "approved", "8 days", "1 day", "now() - interval '8 days'", "now() - interval '7 days 1 minute'")

	accessRequest := func(name, status, created, expires, decided string) {
		exec("access_requests", name, fmt.Sprintf(`INSERT INTO access_requests (id, principal, kind, workspace, verified, change, status, created_at, expires_at, decided_at)
			VALUES (md5($1), 'alice', 'change', $1, true, '{}', '%s', now() - interval '%s', now() - interval '%s', %s)`,
			status, created, expires, decided), name)
	}
	accessRequest("keep-pending", "pending", "1 minute", "-10 minutes", "NULL")
	accessRequest("keep-expired-recently", "expired", "7 days 1 hour", "6 days 23 hours 59 minutes", "NULL")
	accessRequest("drop-expired", "expired", "8 days", "7 days 1 minute", "NULL")
	accessRequest("keep-approved-recently", "approved", "7 days 1 hour", "1 day", "now() - interval '6 days 23 hours 59 minutes'")
	accessRequest("drop-approved", "approved", "8 days", "1 day", "now() - interval '7 days 1 minute'")
	accessRequest("keep-denied-recently", "denied", "7 days 1 hour", "1 day", "now() - interval '6 days 23 hours 59 minutes'")
	accessRequest("drop-denied", "denied", "8 days", "1 day", "now() - interval '7 days 1 minute'")

	uiSession := func(name, created, lastSeen string) {
		exec("ui_sessions", name, fmt.Sprintf(`INSERT INTO ui_sessions (id_hash, principal, csrf_token, created_at, last_seen_at, expires_at)
			VALUES (sha256(convert_to($1::text, 'UTF8')), $1::text, sha256(convert_to($1::text, 'UTF8')), now() - interval '%s', now() - interval '%s', now() - interval '%s' + interval '8 hours')`,
			created, lastSeen, created), name)
	}
	uiSession("keep-valid", "7 hours 59 minutes", "59 minutes")
	uiSession("drop-idle", "1 hour 1 minute", "1 hour 1 minute")
	uiSession("drop-expired", "8 hours 1 minute", "1 minute")

	login := func(name, created string) {
		exec("ui_logins", name, fmt.Sprintf(`INSERT INTO ui_logins (state_hash, nonce, pkce_verifier, created_at)
			VALUES (sha256(convert_to($1::text, 'UTF8')), $1::text, 'v', now() - interval '%s')`, created), name)
	}
	login("keep-recent", "9 minutes 59 seconds")
	login("drop-old", "10 minutes 1 second")

	require.NoError(t, store.DeleteStaleRows(ctx, queries))

	for table, column := range map[string]string{
		"grants": "tool", "temporary_blocks": "reason", "approvals": "tool", "ui_sessions": "principal", "ui_logins": "nonce",
		"access_requests": "workspace",
	} {
		rows, err := pool.Query(ctx, fmt.Sprintf("SELECT %s FROM %s", column, table))
		require.NoError(t, err)
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)

		assert.ElementsMatch(t, kept[table], names, "%s keeps exactly its live and recent rows", table)
	}

	require.NoError(t, store.DeleteStaleRows(ctx, queries), "a second pass deletes nothing and succeeds")
}
