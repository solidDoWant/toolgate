//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// insertMCPSession stores an MCP session of a new toolgate session with the
// given session ID.
func insertMCPSession(t *testing.T, pool *pgxpool.Pool, id, sessionID string) db.McpSession {
	t.Helper()

	queries := db.New(pool)

	session, err := queries.UpsertSession(t.Context(), db.UpsertSessionParams{
		Principal: "alice", SessionID: sessionID, Workspace: "claimed/default", Attestation: "user", Kind: "user",
		Groups: []string{},
	})
	require.NoError(t, err)

	row, err := queries.InsertMCPSession(t.Context(), db.InsertMCPSessionParams{
		ID: id, SessionPk: session.ID, Principal: session.Principal, SessionID: session.SessionID,
		Workspace: session.Workspace, Attestation: session.Attestation,
		ProtocolVersion: "2025-11-25", InitializeParams: []byte(`{"clientInfo":{"name":"claude-code"}}`),
	})
	require.NoError(t, err)

	return row
}

// setMCPSessionTime moves the column of the MCP session to ago before the
// database's clock.
func setMCPSessionTime(t *testing.T, pool *pgxpool.Pool, id, column string, ago time.Duration) {
	t.Helper()

	_, err := pool.Exec(t.Context(), "UPDATE mcp_sessions SET "+column+" = now() - make_interval(secs => $2) WHERE id = $1",
		id, ago.Seconds())
	require.NoError(t, err)
}

func TestMCPSessionRowKeepsBindingAndParams(t *testing.T) {
	_, pool := migratedPool(t)
	queries := db.New(pool)

	inserted := insertMCPSession(t, pool, "0123456789abcdef0123456789abcdef", "helper-session")

	row, err := queries.GetMCPSession(t.Context(), inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, "alice", row.Principal)
	assert.Equal(t, "helper-session", row.SessionID)
	assert.Equal(t, "claimed/default", row.Workspace)
	assert.Equal(t, "user", row.Attestation)
	assert.Equal(t, "2025-11-25", row.ProtocolVersion)
	assert.JSONEq(t, `{"clientInfo":{"name":"claude-code"}}`, string(row.InitializeParams))
	assert.Nil(t, row.InitializedParams, "initialized params are null until the notification arrives")

	updated, err := queries.SetMCPSessionInitializedParams(t.Context(), db.SetMCPSessionInitializedParamsParams{
		ID: row.ID, InitializedParams: []byte(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), updated)

	row, err = queries.GetMCPSession(t.Context(), inserted.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(row.InitializedParams))

	_, err = queries.GetMCPSession(t.Context(), "fedcba9876543210fedcba9876543210")
	require.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestMCPSessionTouchAndIdleEndExcludeEachOther(t *testing.T) {
	idleTimeout := pgtype.Interval{Microseconds: (30 * time.Minute).Microseconds(), Valid: true}

	t.Run("touch first keeps the session", func(t *testing.T) {
		_, pool := migratedPool(t)
		queries := db.New(pool)

		row := insertMCPSession(t, pool, "0123456789abcdef0123456789abcdef", "mcp.0123456789abcdef0123456789abcdef")
		setMCPSessionTime(t, pool, row.ID, "last_seen_at", 31*time.Minute)

		touched, err := queries.TouchMCPSession(t.Context(), row.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), touched)

		_, err = queries.EndIdleConnectorMCPSession(t.Context(), db.EndIdleConnectorMCPSessionParams{ID: row.ID, IdleTimeout: idleTimeout})
		require.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("end first makes the touch match no row", func(t *testing.T) {
		_, pool := migratedPool(t)
		queries := db.New(pool)

		row := insertMCPSession(t, pool, "0123456789abcdef0123456789abcdef", "mcp.0123456789abcdef0123456789abcdef")
		setMCPSessionTime(t, pool, row.ID, "last_seen_at", 31*time.Minute)

		_, err := queries.EndIdleConnectorMCPSession(t.Context(), db.EndIdleConnectorMCPSessionParams{ID: row.ID, IdleTimeout: idleTimeout})
		require.NoError(t, err)

		touched, err := queries.TouchMCPSession(t.Context(), row.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), touched, "a touch of an ended session matches no live row")
	})
}

func TestDeleteStaleRowsDeletesMCPSessionsAnHourAfterTheyEnded(t *testing.T) {
	_, pool := migratedPool(t)
	queries := db.New(pool)

	old := insertMCPSession(t, pool, "00000000000000000000000000000001", "mcp.00000000000000000000000000000001")
	setMCPSessionTime(t, pool, old.ID, "ended_at", time.Hour+time.Minute)

	recent := insertMCPSession(t, pool, "00000000000000000000000000000002", "mcp.00000000000000000000000000000002")
	setMCPSessionTime(t, pool, recent.ID, "ended_at", time.Hour-time.Minute)

	live := insertMCPSession(t, pool, "00000000000000000000000000000003", "mcp.00000000000000000000000000000003")
	setMCPSessionTime(t, pool, live.ID, "last_seen_at", 2*time.Hour)

	require.NoError(t, store.DeleteStaleRows(t.Context(), queries))

	_, err := queries.GetMCPSession(t.Context(), old.ID)
	require.ErrorIs(t, err, pgx.ErrNoRows, "an MCP session that ended over an hour ago is deleted")

	_, err = queries.GetMCPSession(t.Context(), recent.ID)
	require.NoError(t, err, "an MCP session that ended within the hour stays")

	_, err = queries.GetMCPSession(t.Context(), live.ID)
	require.NoError(t, err, "a live MCP session stays")
}
