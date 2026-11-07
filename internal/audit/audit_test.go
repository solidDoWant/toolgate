package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// capturingStore keeps the rows the recorder writes.
type capturingStore struct {
	rows []db.InsertAuditEventParams
	err  error
}

func (s *capturingStore) InsertAuditEvent(_ context.Context, arg db.InsertAuditEventParams) error {
	s.rows = append(s.rows, arg)

	return s.err
}

func fullEvent() audit.Event {
	return audit.Event{
		Type:        audit.TypeToolDecision,
		Principal:   "alice@example.com",
		Workspace:   "github.com/example/repo",
		Session:     "session-1",
		Attestation: "oidc",
		Server:      "test-server",
		Tool:        "echo",
		TraceID:     "0af7651916cd43dd8448eb211c80319c",
		RequestID:   "request-1",
		Details:     map[string]any{"decision": "allow", "rule": "default"},
	}
}

func decodeLines(t *testing.T, output string) []map[string]any {
	t.Helper()

	var lines []map[string]any

	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}

		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &decoded), "line %q", line)
		lines = append(lines, decoded)
	}

	return lines
}

func TestRecordWritesOneLineAndOneRow(t *testing.T) {
	var stdout bytes.Buffer

	store := &capturingStore{}
	recorder := audit.NewRecorder(&stdout, store)
	event := fullEvent()

	require.NoError(t, recorder.Record(t.Context(), event))

	lines := decodeLines(t, stdout.String())
	require.Len(t, lines, 1)
	line := lines[0]
	assert.Equal(t, "audit", line["msg"])
	assert.Equal(t, event.Type, line["type"])
	assert.Equal(t, event.Principal, line["principal"])
	assert.Equal(t, event.Workspace, line["workspace"])
	assert.Equal(t, event.Session, line["session"])
	assert.Equal(t, event.Attestation, line["attestation"])
	assert.Equal(t, event.Server, line["server"])
	assert.Equal(t, event.Tool, line["tool"])
	assert.Equal(t, event.TraceID, line["trace_id"])
	assert.Equal(t, event.RequestID, line["request_id"])
	assert.Equal(t, map[string]any{"decision": "allow", "rule": "default"}, line["details"])

	require.Len(t, store.rows, 1)
	row := store.rows[0]
	assert.Equal(t, db.InsertAuditEventParams{
		Type:        event.Type,
		Principal:   event.Principal,
		Workspace:   event.Workspace,
		Session:     event.Session,
		Attestation: event.Attestation,
		Server:      event.Server,
		Tool:        event.Tool,
		TraceID:     event.TraceID,
		RequestID:   event.RequestID,
		Details:     row.Details,
	}, row)
	assert.JSONEq(t, `{"decision":"allow","rule":"default"}`, string(row.Details))
	assert.Nil(t, row.Payload, "the audit row holds no payload")
}

func TestRecordTakesTraceIDFromContext(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)

	ctx := trace.ContextWithSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))

	var stdout bytes.Buffer

	store := &capturingStore{}
	event := fullEvent()
	event.TraceID = ""

	require.NoError(t, audit.NewRecorder(&stdout, store).Record(ctx, event))

	lines := decodeLines(t, stdout.String())
	require.Len(t, lines, 1)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", lines[0]["trace_id"])
	require.Len(t, store.rows, 1)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", store.rows[0].TraceID)
}

func TestRecordEmptyDetailsAreAnObject(t *testing.T) {
	var stdout bytes.Buffer

	store := &capturingStore{}
	event := fullEvent()
	event.Details = nil

	require.NoError(t, audit.NewRecorder(&stdout, store).Record(t.Context(), event))
	require.Len(t, store.rows, 1)
	assert.JSONEq(t, `{}`, string(store.rows[0].Details))
}

func TestRecordReturnsStoreError(t *testing.T) {
	var stdout bytes.Buffer

	store := &capturingStore{err: fmt.Errorf("database down")}

	err := audit.NewRecorder(&stdout, store).Record(t.Context(), fullEvent())
	require.ErrorContains(t, err, "database down")
}

func TestRecordRefusesEmptyType(t *testing.T) {
	var stdout bytes.Buffer

	store := &capturingStore{}
	event := fullEvent()
	event.Type = ""

	require.Error(t, audit.NewRecorder(&stdout, store).Record(t.Context(), event))
	assert.Empty(t, stdout.String())
	assert.Empty(t, store.rows)
}

// knownSecret is a value that must never reach a log line or a row.
const knownSecret = "ghp_known-secret-value-7c1f0e"

type credentials struct {
	User        string `json:"user"`
	AccessToken string `json:"access_token"`
}

func TestRecordNeverWritesSecretsOrTokens(t *testing.T) {
	tests := []struct {
		name    string
		details map[string]any
	}{
		{name: "secret value", details: map[string]any{"name": "github-token", "value": knownSecret}},
		{name: "secret_value", details: map[string]any{"secret_value": knownSecret}},
		{name: "bearer token", details: map[string]any{"token": knownSecret}},
		{name: "access token", details: map[string]any{"access_token": knownSecret}},
		{name: "refresh token", details: map[string]any{"refreshToken": knownSecret}},
		{name: "id token", details: map[string]any{"id-token": knownSecret}},
		{name: "authorization header", details: map[string]any{"Authorization": "Bearer " + knownSecret}},
		{name: "cookie", details: map[string]any{"Cookie": knownSecret}},
		{name: "password", details: map[string]any{"password": knownSecret}},
		{name: "client secret", details: map[string]any{"client_secret": knownSecret}},
		{name: "api key", details: map[string]any{"api_key": knownSecret}},
		{name: "tool arguments", details: map[string]any{"arguments": map[string]any{"query": knownSecret}}},
		{name: "tool result", details: map[string]any{"result": knownSecret}},
		{name: "nested map", details: map[string]any{"request": map[string]any{"headers": map[string]any{"authorization": knownSecret}}}},
		{name: "map in list", details: map[string]any{"attempts": []any{map[string]any{"token": knownSecret}}}},
		{name: "string map", details: map[string]any{"headers": map[string]string{"Authorization": knownSecret}}},
		{name: "struct", details: map[string]any{"login": credentials{User: "alice", AccessToken: knownSecret}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer

			store := &capturingStore{}
			event := fullEvent()
			event.Details = test.details

			require.NoError(t, audit.NewRecorder(&stdout, store).Record(t.Context(), event))

			require.Len(t, decodeLines(t, stdout.String()), 1)
			assert.NotContains(t, stdout.String(), knownSecret)
			assert.Contains(t, stdout.String(), "[REDACTED]")

			require.Len(t, store.rows, 1)
			assert.NotContains(t, fmt.Sprintf("%+v", store.rows[0]), knownSecret)
			assert.NotContains(t, string(store.rows[0].Details), knownSecret)
		})
	}
}

func TestRecordKeepsOrdinaryDetails(t *testing.T) {
	var stdout bytes.Buffer

	store := &capturingStore{}
	event := fullEvent()
	event.Type = audit.TypeSecretSet
	event.Details = map[string]any{"name": "github-token", "duration_ms": 12, "outcome": "success"}

	require.NoError(t, audit.NewRecorder(&stdout, store).Record(t.Context(), event))
	require.Len(t, store.rows, 1)
	assert.JSONEq(t, `{"name":"github-token","duration_ms":12,"outcome":"success"}`, string(store.rows[0].Details))
}
