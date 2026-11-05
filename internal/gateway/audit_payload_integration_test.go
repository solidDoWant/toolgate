//go:build integration

package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/gateway"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
)

// failingPuts is an S3 client whose puts always fail.
type failingPuts struct {
	audit.S3API
}

func (failingPuts) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return nil, errors.New("S3 is unavailable")
}

// payloadRow is the payload columns of one audit row.
type payloadRow struct {
	Payload    []byte
	PayloadRef string
	Details    map[string]any
}

func (f *mcpFixture) payloadRows(t *testing.T, eventType string) []payloadRow {
	t.Helper()

	rows, err := f.pool.Query(t.Context(),
		`SELECT payload, payload_ref, details FROM audit_events WHERE type = $1 ORDER BY id`, eventType)
	require.NoError(t, err)

	defer rows.Close()

	var collected []payloadRow

	for rows.Next() {
		var row payloadRow
		require.NoError(t, rows.Scan(&row.Payload, &row.PayloadRef, &row.Details))
		collected = append(collected, row)
	}

	require.NoError(t, rows.Err())

	return collected
}

func newPayloads(t *testing.T, client audit.S3API, bucket string) *audit.Payloads {
	t.Helper()

	payloads, err := audit.NewPayloads(t.Context(), client, audit.PayloadOptions{Bucket: bucket, Prefix: "audit/"})
	require.NoError(t, err)

	return payloads
}

func withPayloads(payloads *audit.Payloads) func(*gateway.MCPOptions) {
	return func(options *gateway.MCPOptions) { options.AuditPayloads = payloads }
}

func TestCallAuditPayloads(t *testing.T) {
	t.Run("with payloads on, small arguments and results are stored inline", func(t *testing.T) {
		server := toolgatetest.StartS3(t)
		fixture := newMCPFixture(t, withPayloads(newPayloads(t, server.Client(), server.Bucket)))
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "hello"})
		require.False(t, result.IsError, resultText(result))

		decisions := fixture.payloadRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.JSONEq(t, `{"text":"hello"}`, string(decisions[0].Payload))
		assert.Empty(t, decisions[0].PayloadRef)

		results := fixture.payloadRows(t, "tool.result")
		require.Len(t, results, 1)
		assert.JSONEq(t, `{"content":[{"type":"text","text":"hello"}]}`, string(results[0].Payload))
		assert.Empty(t, results[0].PayloadRef)
	})

	t.Run("with payloads on, a large result is stored in S3 and referenced from the row", func(t *testing.T) {
		server := toolgatetest.StartS3(t)
		fixture := newMCPFixture(t, withPayloads(newPayloads(t, server.Client(), server.Bucket)))
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "sized")

		result := callTool(t, client.session, "github__sized", map[string]any{"size": 100_000})
		require.False(t, result.IsError, resultText(result))

		results := fixture.payloadRows(t, "tool.result")
		require.Len(t, results, 1)
		assert.Nil(t, results[0].Payload)
		assert.Regexp(t, `^audit/payloads/\d{4}/\d{2}/\d{2}/[0-9a-f-]{36}\.json$`, results[0].PayloadRef)

		object, err := server.Client().GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String(server.Bucket), Key: aws.String(results[0].PayloadRef),
		})
		require.NoError(t, err)

		body, err := io.ReadAll(object.Body)
		require.NoError(t, err)
		require.NoError(t, object.Body.Close())

		var stored struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		require.NoError(t, json.Unmarshal(body, &stored))
		require.Len(t, stored.Content, 1)
		assert.Equal(t, resultText(result), stored.Content[0].Text)
	})

	t.Run("with payloads off, nothing is stored", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		callTool(t, client.session, "github__echo", map[string]any{"text": "hello"})

		for _, eventType := range []string{"tool.decision", "tool.result"} {
			rows := fixture.payloadRows(t, eventType)
			require.Len(t, rows, 1)
			assert.Nil(t, rows[0].Payload, eventType)
			assert.Empty(t, rows[0].PayloadRef, eventType)
		}
	})

	t.Run("arguments that can't be stored refuse the call before it is forwarded", func(t *testing.T) {
		server := toolgatetest.StartS3(t)
		payloads := newPayloads(t, failingPuts{server.Client()}, server.Bucket)
		fixture := newMCPFixture(t, withPayloads(payloads))
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "slow")

		result := callTool(t, client.session, "github__slow", map[string]any{"padding": strings.Repeat("a", audit.InlinePayloadLimit)})
		assert.True(t, result.IsError)
		assert.Equal(t, "The call couldn't be recorded. Try again later.", resultText(result))
		assert.Empty(t, upstream.started, "the call reached the upstream server")

		decisions := fixture.payloadRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, "payload-error", decisions[0].Details["reason"])
		assert.Empty(t, fixture.payloadRows(t, "tool.result"))
		assert.InDelta(t, 1, testutil.ToFloat64(payloads.Collectors()[0]), 0)
	})

	t.Run("a result that can't be stored is still returned and the row records payload_error", func(t *testing.T) {
		server := toolgatetest.StartS3(t)
		payloads := newPayloads(t, failingPuts{server.Client()}, server.Bucket)
		fixture := newMCPFixture(t, withPayloads(payloads))
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "sized")

		result := callTool(t, client.session, "github__sized", map[string]any{"size": 100_000})
		require.False(t, result.IsError, resultText(result))
		assert.Len(t, resultText(result), len(resultText(sizedResult(100_000))))

		results := fixture.payloadRows(t, "tool.result")
		require.Len(t, results, 1)
		assert.Nil(t, results[0].Payload)
		assert.Empty(t, results[0].PayloadRef)
		assert.Contains(t, results[0].Details["payload_error"], "S3 is unavailable")
		assert.InDelta(t, 1, testutil.ToFloat64(payloads.Collectors()[0]), 0)
	})
}
