//go:build integration

package audit_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
)

// refusingDeletes is an S3 client that refuses every DeleteObjects request
// holding one of its keys.
type refusingDeletes struct {
	*s3.Client
	refuse map[string]bool
}

func (r refusingDeletes) DeleteObjects(ctx context.Context, input *s3.DeleteObjectsInput, options ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	for _, object := range input.Delete.Objects {
		if r.refuse[aws.ToString(object.Key)] {
			return nil, errors.New("access denied")
		}
	}

	return r.Client.DeleteObjects(ctx, input, options...)
}

func objectExists(t *testing.T, server *toolgatetest.S3, key string) bool {
	t.Helper()

	_, err := server.Client().HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(server.Bucket), Key: aws.String(key)})
	if err == nil {
		return true
	}

	var apiError smithy.APIError
	require.ErrorAs(t, err, &apiError)

	return false
}

func TestPayloadsStoreObjectsBeforeRows(t *testing.T) {
	server := toolgatetest.StartS3(t)
	pool := migratedPool(t)

	payloads, err := audit.NewPayloads(t.Context(), server.Client(), audit.PayloadOptions{Bucket: server.Bucket, Prefix: "tg/"})
	require.NoError(t, err)
	assert.False(t, payloads.Locked(), "a bucket without object lock")

	event := audit.Event{Type: audit.TypeToolResult}
	large := bytes.Repeat([]byte("a"), audit.InlinePayloadLimit)
	require.NoError(t, payloads.Attach(t.Context(), &event, string(large)))
	require.NoError(t, audit.NewRecorder(&bytes.Buffer{}, db.New(pool)).Record(t.Context(), event))

	var ref string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT payload_ref FROM audit_events").Scan(&ref))
	assert.Equal(t, event.PayloadRef, ref)
	assert.True(t, objectExists(t, server, ref))
}

func TestRetentionDeletesPayloadObjectsBeforeRows(t *testing.T) {
	server := toolgatetest.StartS3(t)
	pool := migratedPool(t)
	ctx := t.Context()

	now := time.Now().UTC()
	age := 90 * 24 * time.Hour
	cutoff := now.Add(-age)

	for month := cutoff.AddDate(0, -2, 0); month.Before(now); month = month.AddDate(0, 1, 0) {
		require.NoError(t, store.CreateAuditPartitions(ctx, pool, month))
	}

	put := func(key string) {
		_, err := server.Client().PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(server.Bucket), Key: aws.String(key), Body: bytes.NewReader([]byte(`{}`)),
		})
		require.NoError(t, err)
	}

	insert := func(occurredAt time.Time, eventType, ref string) {
		_, err := pool.Exec(ctx, "INSERT INTO audit_events (occurred_at, type, payload_ref) VALUES ($1, $2, $3)",
			occurredAt, eventType, ref)
		require.NoError(t, err)
	}

	put("tg/stored-1.json")
	put("tg/refused.json")
	put("tg/stored-2.json")

	// With a batch size of 2 the pages are [stored-1, missing],
	// [refused, no-payload] and [stored-2].
	insert(cutoff.Add(-5*time.Hour), "stored-1", "tg/stored-1.json")
	insert(cutoff.Add(-4*time.Hour), "missing", "tg/missing.json")
	insert(cutoff.Add(-3*time.Hour), "refused", "tg/refused.json")
	insert(cutoff.Add(-2*time.Hour), "no-payload", "")
	insert(cutoff.Add(-1*time.Hour), "stored-2", "tg/stored-2.json")
	insert(now, "recent", "")

	client := refusingDeletes{Client: server.Client(), refuse: map[string]bool{"tg/refused.json": true}}
	payloads, err := audit.NewPayloads(ctx, client, audit.PayloadOptions{Bucket: server.Bucket, Prefix: "tg/"})
	require.NoError(t, err)

	queries := db.New(pool)
	retention := audit.NewRetention(queries, age, nil)
	retention.Rows = queries
	retention.Objects = payloads
	retention.BatchSize = 2
	retention.Now = func() time.Time { return now }

	remaining := func() []string {
		rows, err := pool.Query(ctx, "SELECT type FROM audit_events ORDER BY occurred_at")
		require.NoError(t, err)

		defer rows.Close()

		var types []string

		for rows.Next() {
			var eventType string
			require.NoError(t, rows.Scan(&eventType))
			types = append(types, eventType)
		}

		require.NoError(t, rows.Err())

		return types
	}

	deleted, err := retention.DeleteExpired(ctx)
	require.ErrorContains(t, err, "access denied", "a refused delete is reported")
	assert.Equal(t, int64(4), deleted)
	assert.Equal(t, []string{"refused", "recent"}, remaining(), "the refused row is kept and later rows are still deleted")
	assert.InDelta(t, 1, testutil.ToFloat64(retention.Refused), 0)
	assert.False(t, objectExists(t, server, "tg/stored-1.json"))
	assert.False(t, objectExists(t, server, "tg/stored-2.json"))
	assert.True(t, objectExists(t, server, "tg/refused.json"), "no object is deleted without its row")

	// The next run, with S3 accepting, deletes the kept row and its object.
	delete(client.refuse, "tg/refused.json")

	deleted, err = retention.DeleteExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	assert.Equal(t, []string{"recent"}, remaining())
	assert.False(t, objectExists(t, server, "tg/refused.json"))
}
