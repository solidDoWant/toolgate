//go:build integration

package ui_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// payloadEnv is a UI env with payloads on and a real S3 server.
func startPayloadEnv(t *testing.T) (*uiEnv, *audit.Payloads) {
	t.Helper()

	server := toolgatetest.StartS3(t)

	payloads, err := audit.NewPayloads(t.Context(), server.Client(), audit.PayloadOptions{Bucket: server.Bucket, Prefix: "tg/"})
	require.NoError(t, err)

	env := startUIEnvConfigured(t, "admins", toolgatetest.OIDCOptions{}, func(options *ui.Options) {
		options.AuditPayloads = true
		options.Payloads = payloads
	})

	return env, payloads
}

// insertPayloadEvent stores an event of principal whose payload goes where
// audit.Payloads.Attach puts it, and returns its page key.
func (e *uiEnv) insertPayloadEvent(t *testing.T, payloads *audit.Payloads, eventType, principal string, payload any, details string) string {
	t.Helper()

	event := audit.Event{}
	require.NoError(t, payloads.Attach(t.Context(), &event, payload))

	at := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, store.EnsureAuditPartitions(t.Context(), e.pool, at))

	var inline any
	if event.Payload != nil {
		inline = []byte(event.Payload)
	}

	var id int64

	err := e.pool.QueryRow(t.Context(),
		`INSERT INTO audit_events (occurred_at, type, principal, details, payload, payload_ref)
		 VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6) RETURNING id`,
		pgtype.Timestamptz{Time: at, Valid: true}, eventType, principal, details, inline, event.PayloadRef).Scan(&id)
	require.NoError(t, err)

	return fmt.Sprintf("%d-%d", at.UnixMicro(), id)
}

// insertInlinePayloadEvent stores an event whose inline payload is raw JSON
// kept byte for byte (Attach would escape angle brackets), and returns its
// page key.
func (e *uiEnv) insertInlinePayloadEvent(t *testing.T, principal, raw string) string {
	t.Helper()

	at := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, store.EnsureAuditPartitions(t.Context(), e.pool, at))

	var id int64

	err := e.pool.QueryRow(t.Context(),
		`INSERT INTO audit_events (occurred_at, type, principal, details, payload)
		 VALUES ($1, 'tool.result', $2, '{}', $3::jsonb) RETURNING id`, at, principal, raw).Scan(&id)
	require.NoError(t, err)

	return fmt.Sprintf("%d-%d", at.UnixMicro(), id)
}

func TestAuditEventPayloadView(t *testing.T) {
	env, payloads := startPayloadEnv(t)

	// A payload over the inline limit goes to S3; one under it stays inline.
	big := strings.Repeat("<b>x", audit.InlinePayloadLimit/4+10)
	bigKey := env.insertPayloadEvent(t, payloads, "tool.result", "dave", map[string]any{"text": big}, `{}`)
	smallKey := env.insertInlinePayloadEvent(t, "dave", `{"text":"<script>alert(1)</script>"}`)
	noneKey := env.insertAudit(t, auditRow{at: time.Now().UTC().Add(-time.Minute), principal: "dave", tool: "plain"})

	user := env.insertSession(t, "payload-user", nil)
	stranger := env.insertSessionOf(t, "erin", "payload-stranger", nil)
	admin := env.insertSession(t, "payload-admin", []string{"admins"})

	t.Run("small payload is shown escaped", func(t *testing.T) {
		page := readBody(t, get(t, user, env.server.URL+"/ui/audit/events/"+smallKey))
		assert.Contains(t, page, "&lt;script&gt;alert(1)&lt;/script&gt;")
		assert.NotContains(t, page, "<script>alert(1)")
	})

	t.Run("an S3 payload is streamed and cut at 1 MiB", func(t *testing.T) {
		response := get(t, admin, env.server.URL+"/ui/audit/events/"+bigKey)
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, `\u003cb\u003ex`)
		assert.Contains(t, page, "Download the payload")
		assert.NotContains(t, page, "Only the first 1 MiB", "a payload under 1 MiB is shown whole")
	})

	t.Run("an event without a payload has no download", func(t *testing.T) {
		page := readBody(t, get(t, user, env.server.URL+"/ui/audit/events/"+noneKey))
		assert.NotContains(t, page, "Download the payload")
		assert.Equal(t, http.StatusNotFound, get(t, user, env.server.URL+"/ui/audit/events/"+noneKey+"/payload").StatusCode)
	})

	t.Run("download headers and the whole body", func(t *testing.T) {
		for _, key := range []string{smallKey, bigKey} {
			response := get(t, user, env.server.URL+"/ui/audit/events/"+key+"/payload")
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "application/json", response.Header.Get("Content-Type"))
			assert.True(t, strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment"))
			assert.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
			assert.Contains(t, readBody(t, response), `"text":`)
		}

		body := readBody(t, get(t, user, env.server.URL+"/ui/audit/events/"+bigKey+"/payload"))
		assert.Greater(t, len(body), audit.InlinePayloadLimit)
	})

	t.Run("others get 404", func(t *testing.T) {
		for _, suffix := range []string{"", "/payload"} {
			response := get(t, stranger, env.server.URL+"/ui/audit/events/"+bigKey+suffix)
			assert.Equal(t, http.StatusNotFound, response.StatusCode, suffix)
			assert.NotContains(t, readBody(t, response), `u003cb`)
		}
	})
}

func TestAuditEventPayloadIsCutAtOneMiB(t *testing.T) {
	env, payloads := startPayloadEnv(t)

	key := env.insertPayloadEvent(t, payloads, "tool.result", "dave", strings.Repeat("a", ui.MaxShownPayloadBytes+100), `{}`)
	user := env.insertSession(t, "cut-user", nil)

	page := readBody(t, get(t, user, env.server.URL+"/ui/audit/events/"+key))
	assert.Contains(t, page, "Only the first 1 MiB is shown.")
	assert.Less(t, len(page), ui.MaxShownPayloadBytes+10_000)

	body := readBody(t, get(t, user, env.server.URL+"/ui/audit/events/"+key+"/payload"))
	assert.Greater(t, len(body), ui.MaxShownPayloadBytes+100, "the download is whole")
}

func TestApprovalPageShowsArguments(t *testing.T) {
	env, payloads := startPayloadEnv(t)
	id := approvalID(0)
	env.insertApproval(t, id, "dave", approvalTimeout)
	env.insertPayloadEvent(t, payloads, "approval.request", "dave", map[string]any{"repo": "<acme/repo>"}, fmt.Sprintf(`{"approval":%q}`, id))
	env.insertPayloadEvent(t, payloads, "approval.request", "dave", map[string]any{"repo": "other-approval"}, fmt.Sprintf(`{"approval":%q}`, approvalID(1)))

	browser := env.insertSession(t, "args-session", nil)

	page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
	assert.Contains(t, page, "&lt;acme/repo&gt;")
	assert.NotContains(t, page, "other-approval")
	assert.NotContains(t, page, "audit payloads are off")
}
