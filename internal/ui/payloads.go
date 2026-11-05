package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// MaxShownPayloadBytes is how much of a payload a page shows
// (docs/design/phase-2.md §8).
const MaxShownPayloadBytes = 1 << 20

const (
	payloadUnavailableNote = "The payload can't be read right now. Try again later."
	payloadsOffAuditNote   = "Payloads are off, so this event has no payload to show."
)

// PayloadSource reads payload objects. *audit.Payloads implements it.
type PayloadSource interface {
	// Open returns the body of the payload object with key.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// shownPayload is the part of a payload a page shows.
type shownPayload struct {
	text      string
	truncated bool
	note      string
}

// readShown returns the first MaxShownPayloadBytes of a payload, held inline
// or in S3. found is false when there is no payload.
func (u *UI) readShown(ctx context.Context, inline []byte, ref string) (shownPayload, bool) {
	if ref == "" {
		if len(inline) == 0 {
			return shownPayload{}, false
		}

		return shownBytes(inline), true
	}

	if u.options.Payloads == nil {
		return shownPayload{note: payloadsOffAuditNote}, true
	}

	body, err := u.options.Payloads.Open(ctx, ref)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot open an audit payload for the web UI", "error", err)

		return shownPayload{note: payloadUnavailableNote}, true
	}

	defer func() { _ = body.Close() }()

	content, err := io.ReadAll(io.LimitReader(body, MaxShownPayloadBytes+1))
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read an audit payload for the web UI", "error", err)

		return shownPayload{note: payloadUnavailableNote}, true
	}

	return shownBytes(content), true
}

func shownBytes(content []byte) shownPayload {
	shown := shownPayload{}

	if len(content) > MaxShownPayloadBytes {
		content = content[:MaxShownPayloadBytes]
		shown.truncated = true
	}

	var indented bytes.Buffer
	if !shown.truncated && json.Indent(&indented, content, "", "  ") == nil {
		content = indented.Bytes()
	}

	shown.text = strings.ToValidUTF8(string(content), "�")

	return shown
}

// attachPayload fills in the payload of the event page.
func (u *UI) attachPayload(ctx context.Context, event *auditEvent, occurredAt time.Time, id int64) {
	row, err := u.queries.GetAuditEventPayload(ctx, db.GetAuditEventPayloadParams{
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true}, ID: id,
	})
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read an audit payload for the web UI", "error", err)

		event.PayloadNote = payloadUnavailableNote

		return
	}

	shown, found := u.readShown(ctx, row.Payload, row.PayloadRef)
	event.HasPayload = found && shown.note == ""
	event.Payload, event.PayloadTruncated, event.PayloadNote = shown.text, shown.truncated, shown.note
}

// auditEventPayload downloads the whole payload of an event to its
// principal or an admin; anyone else gets 404. Objects stream from S3
// through the gateway.
func (u *UI) auditEventPayload(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	occurredAt, id, ok := parseEventKey(request.PathValue("key"))
	if !ok {
		http.NotFound(writer, request)

		return
	}

	row, err := u.queries.GetAuditEventPayload(ctx, db.GetAuditEventPayloadParams{
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true}, ID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !session.Admin && row.Principal != session.Principal) ||
		(err == nil && row.PayloadRef == "" && len(row.Payload) == 0) {
		http.NotFound(writer, request)

		return
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read an audit payload for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the audit log. Try again later.", http.StatusInternalServerError)

		return
	}

	var body io.Reader = bytes.NewReader(row.Payload)

	if row.PayloadRef != "" {
		if u.options.Payloads == nil {
			http.NotFound(writer, request)

			return
		}

		object, err := u.options.Payloads.Open(ctx, row.PayloadRef)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot open an audit payload for the web UI", "error", err)
			http.Error(writer, payloadUnavailableNote, http.StatusBadGateway)

			return
		}

		defer func() { _ = object.Close() }()

		body = object
	}

	header := writer.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Disposition", `attachment; filename="audit-payload-`+request.PathValue("key")+`.json"`)
	header.Set("X-Content-Type-Options", "nosniff")

	if _, err := io.Copy(writer, body); err != nil {
		u.logger.ErrorContext(ctx, "cannot stream an audit payload for the web UI", "error", err)
	}
}

// approvalArguments reads the arguments from the approval.request payload.
func (u *UI) approvalArguments(ctx context.Context, approvalID string, createdAt time.Time) shownPayload {
	row, err := u.queries.GetApprovalRequestPayload(ctx, db.GetApprovalRequestPayloadParams{
		Since:      pgtype.Timestamptz{Time: createdAt.Add(-time.Minute), Valid: true},
		ApprovalID: approvalID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return shownPayload{note: "The arguments weren't recorded."}
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read the arguments of an approval for the web UI", "error", err)

		return shownPayload{note: payloadUnavailableNote}
	}

	shown, found := u.readShown(ctx, row.Payload, row.PayloadRef)
	if !found {
		return shownPayload{note: "The arguments weren't recorded."}
	}

	return shown
}
