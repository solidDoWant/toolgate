//go:build integration

package audit_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	return pool
}

func TestRecordWritesRowToPostgres(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()

	var stdout bytes.Buffer

	event := fullEvent()
	require.NoError(t, audit.NewRecorder(&stdout, db.New(pool)).Record(ctx, event))

	var row struct {
		eventType, principal, workspace, session, attestation, server, tool, traceID, requestID string
		details                                                                                 []byte
	}
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT type, principal, workspace, session, attestation, server, tool, trace_id, request_id, details::text
		FROM audit_events`).Scan(
		&row.eventType, &row.principal, &row.workspace, &row.session, &row.attestation,
		&row.server, &row.tool, &row.traceID, &row.requestID, &row.details))

	assert.Equal(t, event.Type, row.eventType)
	assert.Equal(t, event.Principal, row.principal)
	assert.Equal(t, event.Workspace, row.workspace)
	assert.Equal(t, event.Session, row.session)
	assert.Equal(t, event.Attestation, row.attestation)
	assert.Equal(t, event.Server, row.server)
	assert.Equal(t, event.Tool, row.tool)
	assert.Equal(t, event.TraceID, row.traceID)
	assert.Equal(t, event.RequestID, row.requestID)
	assert.JSONEq(t, `{"decision":"allow","rule":"default"}`, string(row.details))
	assert.Len(t, decodeLines(t, stdout.String()), 1)
}

func TestRecordNeverWritesSecretToPostgres(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()

	var stdout bytes.Buffer

	event := fullEvent()
	event.Type = audit.TypeSecretSet
	event.Details = map[string]any{
		"name":          "github-token",
		"value":         knownSecret,
		"authorization": "Bearer " + knownSecret,
		"arguments":     map[string]any{"query": knownSecret},
	}
	require.NoError(t, audit.NewRecorder(&stdout, db.New(pool)).Record(ctx, event))

	var rows []string

	result, err := pool.Query(ctx, "SELECT audit_events::text FROM audit_events")
	require.NoError(t, err)

	for result.Next() {
		var row string
		require.NoError(t, result.Scan(&row))
		rows = append(rows, row)
	}

	require.NoError(t, result.Err())
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0], knownSecret)
	assert.Contains(t, rows[0], "github-token")
	assert.NotContains(t, stdout.String(), knownSecret)
}

func TestRetentionDeletesExpiredRowsFromPostgres(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()

	now := time.Now().UTC()
	age := 90 * 24 * time.Hour
	cutoff := now.Add(-age)

	// Partitions for the months that hold the old rows.
	for month := cutoff.AddDate(0, -2, 0); month.Before(now); month = month.AddDate(0, 1, 0) {
		require.NoError(t, store.EnsureAuditPartitions(ctx, pool, month))
	}

	insert := func(occurredAt time.Time, eventType string) {
		_, err := pool.Exec(ctx, "INSERT INTO audit_events (occurred_at, type) VALUES ($1, $2)", occurredAt, eventType)
		require.NoError(t, err)
	}

	for index := range 5 {
		insert(cutoff.Add(-time.Duration(index+1)*24*time.Hour), "expired")
	}

	insert(cutoff, "at-cutoff")
	insert(cutoff.Add(time.Minute), "kept")
	insert(now, "kept")

	retention := audit.NewRetention(db.New(pool), age, nil)
	retention.BatchSize = 2
	retention.Now = func() time.Time { return now }

	deleted, err := retention.DeleteExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), deleted)

	var remaining []string

	result, err := pool.Query(ctx, "SELECT type FROM audit_events ORDER BY occurred_at")
	require.NoError(t, err)

	for result.Next() {
		var eventType string
		require.NoError(t, result.Scan(&eventType))
		remaining = append(remaining, eventType)
	}

	require.NoError(t, result.Err())
	assert.Equal(t, []string{"at-cutoff", "kept", "kept"}, remaining)

	// One batch deletes at most BatchSize rows.
	insert(cutoff.Add(-time.Hour), "expired")
	insert(cutoff.Add(-2*time.Hour), "expired")
	insert(cutoff.Add(-3*time.Hour), "expired")

	batch, err := db.New(pool).DeleteAuditEventsBefore(ctx, db.DeleteAuditEventsBeforeParams{
		Cutoff:    pgtypeTime(cutoff),
		BatchSize: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), batch)
}

func pgtypeTime(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
