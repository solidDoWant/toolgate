package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/principal"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Audit page bounds.
const (
	// AuditPageSize is the number of rows on one page of the audit log.
	AuditPageSize = 100
	// DefaultAuditRange is the time range when the request names none.
	DefaultAuditRange = 24 * time.Hour
	// MaxAuditRange is the longest time range one query may cover.
	MaxAuditRange = 31 * 24 * time.Hour

	// maxFilterBytes bounds each filter value.
	maxFilterBytes = 1024
)

// Formats the time fields accept. The first is what a browser's
// datetime-local input sends; all are read as UTC unless they carry a zone.
var auditTimeLayouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05", time.RFC3339, time.RFC3339Nano}

// auditEvent is one audit row as the pages show it.
type auditEvent struct {
	Key        string
	OccurredAt time.Time
	Type       string
	Principal  string
	// Who is Principal as people read it.
	Who            principal.Display
	Workspace      string
	WorkspaceLabel workspaceLabel
	Session        string
	Attestation    string
	Server         string
	Tool           string
	TraceID        string
	RequestID      string
	Details        string
	// Payload is the first part of the payload, empty when there is none.
	Payload string
	// PayloadTruncated is true when Payload is not all of it.
	PayloadTruncated bool
	// PayloadNote explains a payload that can't be shown.
	PayloadNote string
	// HasPayload is true when the event has a payload to download.
	HasPayload bool
}

// auditFilter is the form of the audit page.
type auditFilter struct {
	Principal string
	Workspace string
	Session   string
	Server    string
	Tool      string
	From      time.Time
	To        time.Time
}

type auditPage struct {
	Filter auditFilter
	// CanFilterPrincipal is true for admins; others see only their own.
	CanFilterPrincipal bool
	// FromInput and ToInput fill the time fields of the form.
	FromInput string
	ToInput   string
	Events    []auditEvent
	// NextURL is the link to the next page, empty on the last one.
	NextURL string
}

func eventKey(occurredAt time.Time, id int64) string {
	return strconv.FormatInt(occurredAt.UnixMicro(), 10) + "-" + strconv.FormatInt(id, 10)
}

// parseEventKey reads `<Unix microseconds>-<id>`.
func parseEventKey(key string) (time.Time, int64, bool) {
	micros, id, found := strings.Cut(key, "-")
	if !found {
		return time.Time{}, 0, false
	}

	microsValue, err := strconv.ParseInt(micros, 10, 64)
	if err != nil || microsValue < 0 {
		return time.Time{}, 0, false
	}

	idValue, err := strconv.ParseInt(id, 10, 64)
	if err != nil || idValue < 0 {
		return time.Time{}, 0, false
	}

	return time.UnixMicro(microsValue).UTC(), idValue, true
}

func parseAuditTime(value string) (time.Time, error) {
	for _, layout := range auditTimeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("%q is not a time", value)
}

var errAuditRange = errors.New("the time range must be positive and at most 31 days")

// parseAuditFilter reads the query. A non-admin's principal is always their
// own and the parameter is ignored.
func parseAuditFilter(query url.Values, session Session, now time.Time) (auditFilter, error) {
	for _, name := range []string{"principal", "workspace", "session", "server", "tool"} {
		if len(query.Get(name)) > maxFilterBytes {
			return auditFilter{}, fmt.Errorf("the %s filter is too long", name)
		}
	}

	filter := auditFilter{
		Principal: query.Get("principal"),
		Workspace: query.Get("workspace"),
		Session:   query.Get("session"),
		Server:    query.Get("server"),
		Tool:      query.Get("tool"),
		To:        now.UTC().Truncate(time.Microsecond),
	}

	if !session.Admin {
		filter.Principal = session.Principal
	}

	if value := query.Get("to"); value != "" {
		parsed, err := parseAuditTime(value)
		if err != nil {
			return auditFilter{}, err
		}

		filter.To = parsed
	}

	filter.From = filter.To.Add(-DefaultAuditRange)

	if value := query.Get("from"); value != "" {
		parsed, err := parseAuditTime(value)
		if err != nil {
			return auditFilter{}, err
		}

		filter.From = parsed
	}

	if !filter.From.Before(filter.To) || filter.To.Sub(filter.From) > MaxAuditRange {
		return auditFilter{}, errAuditRange
	}

	return filter, nil
}

func (f auditFilter) values() url.Values {
	values := url.Values{
		"from": {f.From.Format(time.RFC3339Nano)},
		"to":   {f.To.Format(time.RFC3339Nano)},
	}

	for name, value := range map[string]string{
		"principal": f.Principal, "workspace": f.Workspace, "session": f.Session, "server": f.Server, "tool": f.Tool,
	} {
		if value != "" {
			values.Set(name, value)
		}
	}

	return values
}

func toAuditEvent(row db.GetAuditEventRow) auditEvent {
	var details bytes.Buffer
	if err := json.Indent(&details, row.Details, "", "  "); err != nil {
		details.Reset()
		details.Write(row.Details)
	}

	return auditEvent{
		Key:            eventKey(row.OccurredAt.Time, row.ID),
		OccurredAt:     row.OccurredAt.Time.UTC(),
		Type:           row.Type,
		Principal:      row.Principal,
		Workspace:      row.Workspace,
		WorkspaceLabel: labelWorkspace(row.Workspace),
		Session:        row.Session,
		Attestation:    row.Attestation,
		Server:         row.Server,
		Tool:           row.Tool,
		TraceID:        row.TraceID,
		RequestID:      row.RequestID,
		Details:        details.String(),
	}
}

// audit lists audit events, newest first, 100 a page.
func (u *UI) audit(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()
	query := request.URL.Query()

	filter, err := parseAuditFilter(query, session, time.Now())
	if err != nil {
		http.Error(writer, "The filter is not valid: "+err.Error()+".", http.StatusBadRequest)

		return
	}

	params := db.ListAuditEventsPageParams{
		FromTime:  pgtype.Timestamptz{Time: filter.From, Valid: true},
		ToTime:    pgtype.Timestamptz{Time: filter.To, Valid: true},
		Principal: filter.Principal,
		Workspace: filter.Workspace,
		Session:   filter.Session,
		Server:    filter.Server,
		Tool:      filter.Tool,
		PageSize:  AuditPageSize + 1,
	}

	if before := query.Get("before"); before != "" {
		beforeAt, beforeID, ok := parseEventKey(before)
		if !ok {
			http.Error(writer, "The page marker is not valid.", http.StatusBadRequest)

			return
		}

		params.BeforeAt = pgtype.Timestamptz{Time: beforeAt, Valid: true}
		params.BeforeID = beforeID
	}

	rows, err := u.queries.ListAuditEventsPage(ctx, params)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot list audit events for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the audit log. Try again later.", http.StatusInternalServerError)

		return
	}

	data := auditPage{
		Filter:             filter,
		CanFilterPrincipal: session.Admin,
		FromInput:          filter.From.Format("2006-01-02T15:04"),
		ToInput:            filter.To.Format("2006-01-02T15:04"),
		Events:             make([]auditEvent, 0, min(len(rows), AuditPageSize)),
	}

	if len(rows) > AuditPageSize {
		rows = rows[:AuditPageSize]

		last := rows[len(rows)-1]
		next := filter.values()
		next.Set("before", eventKey(last.OccurredAt.Time, last.ID))
		data.NextURL = "/ui/audit?" + next.Encode()
	}

	principals := make([]string, 0, len(rows))

	for _, row := range rows {
		data.Events = append(data.Events, toAuditEvent(db.GetAuditEventRow(row)))
		principals = append(principals, row.Principal)
	}

	names := u.namesFor(ctx, session.Principal, principals...)
	for index := range data.Events {
		data.Events[index].Who = names.show(data.Events[index].Principal)
	}

	u.render(writer, request, "audit", "Audit", data)
}

// auditEventPage shows one event to its principal or an admin; anyone else
// gets 404, so the key leaks nothing.
func (u *UI) auditEventPage(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	occurredAt, id, ok := parseEventKey(request.PathValue("key"))
	if !ok {
		http.NotFound(writer, request)

		return
	}

	row, err := u.queries.GetAuditEvent(ctx, db.GetAuditEventParams{
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ID:         id,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !session.Admin && row.Principal != session.Principal) {
		http.NotFound(writer, request)

		return
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read an audit event for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the audit log. Try again later.", http.StatusInternalServerError)

		return
	}

	event := toAuditEvent(row)
	event.Who = u.namesFor(ctx, session.Principal, row.Principal).show(row.Principal)
	u.attachPayload(ctx, &event, occurredAt, id)

	u.render(writer, request, "audit_event", "Audit event", event)
}
