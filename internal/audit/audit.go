// Package audit records toolgate's audit events. Each event is
// one JSON line on stdout and one row in the audit_events table. Rows older
// than the retention age are deleted in batches by Retention.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Event types.
const (
	TypeSessionStart  = "session.start"
	TypeSessionEnd    = "session.end"
	TypeAuthRefuse    = "auth.refuse"
	TypeServerEnable  = "server.enable"
	TypeServerDisable = "server.disable"
	TypeInstanceStart = "instance.start"
	TypeInstanceReady = "instance.ready"
	TypeInstanceFail  = "instance.fail"
	TypeInstanceStop  = "instance.stop"
	// TypeInstanceHealth is a change of a ready instance's health.
	TypeInstanceHealth = "instance.health"
	// TypeInstanceReapply is a reapply repair of an unhealthy instance.
	TypeInstanceReapply = "instance.reapply"
	TypeToolDecision    = "tool.decision"
	TypeToolResult      = "tool.result"
	TypeRuleChange      = "rule.change"
	TypeBlockCreate     = "block.create"
	TypeSecretSet       = "secret.set"
	TypeSecretDelete    = "secret.delete"
	// TypeVolumeDelete is the decision to delete a persistent volume.
	TypeVolumeDelete = "volume.delete"
	// TypeCredentialExchange is one token exchange a replica ran for an
	// instance, successful or not.
	TypeCredentialExchange = "credential.exchange"
	logMessage             = "audit"
	redactedPlaceholder    = "[REDACTED]"
)

// Event is one audited action. Fields that don't apply to the action stay
// empty. Details holds the event-specific fields, such as the deciding rule
// or a duration. It never holds tool arguments, tool results, secret values
// or tokens; fields named like them are redacted before they are written.
type Event struct {
	Type        string
	Principal   string
	Workspace   string
	Session     string
	Attestation string
	Server      string
	Tool        string
	// TraceID defaults to the trace ID of the span in the context.
	TraceID   string
	RequestID string
	Details   map[string]any
	// Payload is the event's payload as JSON when it is stored inline, and
	// PayloadRef the S3 key when it is stored as an object. Payloads.Attach
	// sets them; they are never written to stdout.
	Payload    []byte
	PayloadRef string
}

// Store writes audit rows. *db.Queries implements it.
type Store interface {
	InsertAuditEvent(ctx context.Context, arg db.InsertAuditEventParams) error
}

// Recorder writes audit events to stdout and PostgreSQL.
type Recorder struct {
	logger *slog.Logger
	store  Store
}

// NewRecorder returns a Recorder that writes JSON lines to stdout and rows
// to store.
func NewRecorder(stdout io.Writer, store Store) *Recorder {
	return &Recorder{
		logger: slog.New(slog.NewJSONHandler(stdout, nil)),
		store:  store,
	}
}

// Record writes event as one JSON line and one row. An error means the row
// wasn't written; a caller that must record a decision before acting
// must not act on it.
func (r *Recorder) Record(ctx context.Context, event Event) error {
	if event.Type == "" {
		return fmt.Errorf("record audit event: type is empty")
	}

	if event.TraceID == "" {
		if spanContext := trace.SpanContextFromContext(ctx); spanContext.HasTraceID() {
			event.TraceID = spanContext.TraceID().String()
		}
	}

	encodedDetails, err := encodeDetails(event.Details)
	if err != nil {
		return fmt.Errorf("encode audit event %s details: %w", event.Type, err)
	}

	r.logger.LogAttrs(ctx, slog.LevelInfo, logMessage,
		slog.String("type", event.Type),
		slog.String("principal", event.Principal),
		slog.String("workspace", event.Workspace),
		slog.String("session", event.Session),
		slog.String("attestation", event.Attestation),
		slog.String("server", event.Server),
		slog.String("tool", event.Tool),
		slog.String("trace_id", event.TraceID),
		slog.String("request_id", event.RequestID),
		slog.Any("details", json.RawMessage(encodedDetails)),
	)

	err = r.store.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		Type:        event.Type,
		Principal:   event.Principal,
		Workspace:   event.Workspace,
		Session:     event.Session,
		Attestation: event.Attestation,
		Server:      event.Server,
		Tool:        event.Tool,
		TraceID:     event.TraceID,
		RequestID:   event.RequestID,
		Details:     encodedDetails,
		Payload:     event.Payload,
		PayloadRef:  event.PayloadRef,
	})
	if err != nil {
		return fmt.Errorf("write audit event %s: %w", event.Type, err)
	}

	return nil
}

// encodeDetails returns details as a JSON object with sensitive fields
// redacted. Details are converted to plain JSON values first, so fields
// inside structs are redacted by their JSON names too.
func encodeDetails(details map[string]any) ([]byte, error) {
	plain := map[string]any{}

	if len(details) > 0 {
		encoded, err := json.Marshal(details)
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(encoded, &plain); err != nil {
			return nil, err
		}
	}

	return json.Marshal(redactMap(plain))
}
