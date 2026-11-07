package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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
	// maxEventTypeFilters bounds the event types of one filter.
	maxEventTypeFilters = 64
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

// auditRow is an event as the list shows it: the stored fields and the
// readable parts built from them.
type auditRow struct {
	auditEvent
	Summary   auditSummary
	Time      timeView
	ShortSess string
	// SessionPage links to the session page, empty when there is none.
	SessionPage string
	// Verified is true when the workspace ID was verified.
	Verified bool
	// Links filter the list by one value of the row.
	WhoURL, WhereURL, SessionURL, ServerURL, ToolURL, TypeURL string
}

// auditFilter is the form of the audit page.
type auditFilter struct {
	Principal string
	Workspace string
	Session   string
	Server    string
	Tool      string
	// Outcome is "", "refused", "error" or "approval".
	Outcome string
	// Category is "" or the key of one of auditCategories.
	Category string
	// EventTypes narrows to exact event types; with a Category, to the
	// types of the category among them.
	EventTypes []string
	// Range is a preset ("1h", "24h", "today", "7d") or "" for the
	// explicit From and To.
	Range string
	From  time.Time
	To    time.Time
}

type auditPage struct {
	Filter auditFilter
	// CanFilterPrincipal is true for admins; others see only their own.
	CanFilterPrincipal bool
	// FromInput and ToInput fill the custom range fields of the form.
	FromInput string
	ToInput   string
	Events    []auditRow
	// NextURL is the link to the next page, empty on the last one.
	NextURL string
	// Count is the number of matching events.
	Count int64
	// ExportURL downloads the current filter as JSON Lines.
	ExportURL string
	// Pickers hold the values each select offers.
	Servers, Tools, Workspaces, Sessions []option
	Principals                           []option
	Categories                           []option
	// EventTypes lists every event type with its readable name.
	EventTypes []option
	Outcomes   []option
	Ranges     []option
}

// option is one entry of a picker.
type option struct {
	Value, Label string
	Selected     bool
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

// auditRangePresets are the time presets of the audit page.
var auditRangePresets = []option{
	{Value: "1h", Label: "Last hour"}, {Value: "24h", Label: "Last 24 hours"},
	{Value: "today", Label: "Today (UTC)"}, {Value: "7d", Label: "Last 7 days"}, {Value: "custom", Label: "Custom range"},
}

var auditOutcomes = []option{
	{Value: "", Label: "Any outcome"}, {Value: "refused", Label: "Refused"},
	{Value: "error", Label: "Errors"}, {Value: "approval", Label: "Approvals"},
}

// parseAuditFilter reads the query. A non-admin's principal is always their
// own and the parameter is ignored.
func parseAuditFilter(query url.Values, session Session, now time.Time) (auditFilter, error) {
	for _, name := range []string{"principal", "workspace", "session", "server", "tool", "outcome", "category", "range"} {
		if len(query.Get(name)) > maxFilterBytes {
			return auditFilter{}, fmt.Errorf("the %s filter is too long", name)
		}
	}

	filter := auditFilter{
		Principal:  query.Get("principal"),
		Workspace:  query.Get("workspace"),
		Session:    query.Get("session"),
		Server:     query.Get("server"),
		Tool:       query.Get("tool"),
		Outcome:    query.Get("outcome"),
		Category:   query.Get("category"),
		EventTypes: query["type"],
		To:         now.UTC().Truncate(time.Microsecond),
	}

	if !session.Admin {
		filter.Principal = session.Principal
	}

	if !slices.Contains([]string{"", "refused", "error", "approval"}, filter.Outcome) {
		return auditFilter{}, fmt.Errorf("%q is not an outcome", filter.Outcome)
	}

	if len(filter.EventTypes) > maxEventTypeFilters {
		return auditFilter{}, errors.New("too many event types")
	}

	for _, eventType := range filter.EventTypes {
		if !slices.Contains(knownAuditTypes(), eventType) {
			return auditFilter{}, fmt.Errorf("%q is not an event type", eventType)
		}
	}

	if filter.Category != "" && categoryTypes(filter.Category) == nil {
		return auditFilter{}, fmt.Errorf("%q is not an event category", filter.Category)
	}

	if preset := query.Get("range"); preset != "" && preset != "custom" && query.Get("from") == "" && query.Get("to") == "" {
		from, ok := presetStart(preset, filter.To)
		if !ok {
			return auditFilter{}, fmt.Errorf("%q is not a time range", preset)
		}

		filter.Range, filter.From = preset, from

		return filter, nil
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

// presetStart is the start of a preset range that ends at now.
func presetStart(preset string, now time.Time) (time.Time, bool) {
	switch preset {
	case "1h":
		return now.Add(-time.Hour), true
	case "24h":
		return now.Add(-DefaultAuditRange), true
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), true
	case "7d":
		return now.Add(-7 * 24 * time.Hour), true
	default:
		return time.Time{}, false
	}
}

// values encodes the filter. With absolute, or without a preset, the time
// range is spelled out, which keeps a page cursor stable.
func (f auditFilter) values(absolute bool) url.Values {
	values := url.Values{}

	if f.Range != "" && !absolute {
		values.Set("range", f.Range)
	} else {
		values.Set("from", f.From.Format(time.RFC3339Nano))
		values.Set("to", f.To.Format(time.RFC3339Nano))
	}

	for _, eventType := range f.EventTypes {
		values.Add("type", eventType)
	}

	for name, value := range map[string]string{
		"principal": f.Principal, "workspace": f.Workspace, "session": f.Session, "server": f.Server, "tool": f.Tool,
		"outcome": f.Outcome, "category": f.Category,
	} {
		if value != "" {
			values.Set(name, value)
		}
	}

	return values
}

// with is the list URL for the filter with one field changed.
func (f auditFilter) with(name, value string) string {
	values := f.values(false)
	if name == "type" {
		values.Del("type")

		if value != "" {
			values.Set("type", value)
		}

		return "/ui/audit?" + values.Encode()
	}

	if value == "" {
		values.Del(name)
	} else {
		values.Set(name, value)
	}

	return "/ui/audit?" + values.Encode()
}

// queryTypes is the event types the query allows; empty allows all. A
// category and a type list that share no type allow none.
func (f auditFilter) queryTypes() []string {
	categoryTypes := categoryTypes(f.Category)

	switch {
	case len(f.EventTypes) == 0:
		return append([]string{}, categoryTypes...)
	case f.Category == "":
		return append([]string{}, f.EventTypes...)
	}

	allowed := []string{}

	for _, eventType := range f.EventTypes {
		if slices.Contains(categoryTypes, eventType) {
			allowed = append(allowed, eventType)
		}
	}

	if len(allowed) == 0 {
		return []string{"none"}
	}

	return allowed
}

// params builds the query parameters of the filter.
func (f auditFilter) params() db.ListAuditEventsPageParams {
	return db.ListAuditEventsPageParams{
		FromTime:  pgtype.Timestamptz{Time: f.From, Valid: true},
		ToTime:    pgtype.Timestamptz{Time: f.To, Valid: true},
		Principal: f.Principal,
		Workspace: f.Workspace,
		Session:   f.Session,
		Server:    f.Server,
		Tool:      f.Tool,
		Types:     f.queryTypes(),
		Outcome:   f.Outcome,
	}
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

// pickerOptions reads the values of one picker and marks the current one.
// The current value is kept even when the range holds no event with it.
func (u *UI) pickerOptions(ctx context.Context, kind string, filter auditFilter, current string, label func(string) string) []option {
	values, err := u.queries.ListAuditFilterOptions(ctx, db.ListAuditFilterOptionsParams{
		Kind:      kind,
		FromTime:  pgtype.Timestamptz{Time: filter.From, Valid: true},
		ToTime:    pgtype.Timestamptz{Time: filter.To, Valid: true},
		Principal: filter.Principal,
	})
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read audit filter options for the web UI", "error", err)
	}

	if current != "" && !slices.Contains(values, current) {
		values = append(values, current)
	}

	options := []option{{Value: ""}}
	for _, value := range values {
		options = append(options, option{Value: value, Label: label(value), Selected: value == current})
	}

	return options
}

// sessionPages maps session IDs to the links of their pages, for the
// sessions the viewer may open.
func (u *UI) sessionPages(ctx context.Context, viewer Session, sessionIDs []string) map[string]string {
	pages := map[string]string{}
	if len(sessionIDs) == 0 {
		return pages
	}

	rows, err := u.queries.ListSessionPageIDs(ctx, db.ListSessionPageIDsParams{SessionIds: sessionIDs, Principal: map[bool]string{true: "", false: viewer.Principal}[viewer.Admin]})
	if err != nil {
		u.logger.WarnContext(ctx, "cannot look up session pages for the web UI", "error", err)

		return pages
	}

	for _, row := range rows {
		pages[row.SessionID] = "/ui/sessions/" + strconv.FormatInt(row.ID, 10)
	}

	return pages
}

func (u *UI) buildRows(ctx context.Context, rows []db.ListAuditEventsPageRow, filter auditFilter, session Session, now time.Time) []auditRow {
	principals := make([]string, 0, len(rows))
	for _, row := range rows {
		principals = append(principals, row.Principal)
	}

	sessionIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Session != "" {
			sessionIDs = append(sessionIDs, row.Session)
		}
	}

	names := u.namesFor(ctx, session.Principal, principals...)
	pages := u.sessionPages(ctx, session, sessionIDs)
	result := make([]auditRow, 0, len(rows))

	for _, row := range rows {
		event := toAuditEvent(db.GetAuditEventRow(row))
		event.Who = names.show(event.Principal)
		item := auditRow{
			auditEvent:  event,
			Summary:     summarize(event),
			Time:        relativeTime(now, event.OccurredAt),
			ShortSess:   principal.Short(event.Session),
			Verified:    event.Workspace != "" && !strings.HasPrefix(event.Workspace, "claimed/"),
			SessionPage: pages[event.Session],
			TypeURL:     filter.with("type", event.Type),
		}

		if event.Principal != "" && session.Admin {
			item.WhoURL = filter.with("principal", event.Principal)
		}

		if event.Workspace != "" {
			item.WhereURL = filter.with("workspace", event.Workspace)
		}

		if event.Session != "" {
			item.SessionURL = filter.with("session", event.Session)
		}

		if event.Server != "" {
			item.ServerURL = filter.with("server", event.Server)
		}

		if event.Tool != "" {
			item.ToolURL = filter.with("tool", event.Tool)
		}

		result = append(result, item)
	}

	return result
}

// audit lists audit events, newest first, 100 a page.
func (u *UI) audit(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()
	query := request.URL.Query()
	now := time.Now()

	filter, err := parseAuditFilter(query, session, now)
	if err != nil {
		http.Error(writer, "The filter is not valid: "+err.Error()+".", http.StatusBadRequest)

		return
	}

	params := filter.params()
	params.PageSize = AuditPageSize + 1

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
		ExportURL:          "/ui/audit/export?" + filter.values(true).Encode(),
	}

	if len(rows) > AuditPageSize {
		rows = rows[:AuditPageSize]

		last := rows[len(rows)-1]
		next := filter.values(true)
		next.Set("before", eventKey(last.OccurredAt.Time, last.ID))
		data.NextURL = "/ui/audit?" + next.Encode()
	}

	data.Events = u.buildRows(ctx, rows, filter, session, now)

	count, err := u.queries.CountAuditEvents(ctx, filter.countParams())
	if err == nil {
		data.Count = count
	}

	identity := func(value string) string { return value }
	data.Servers = u.pickerOptions(ctx, "server", filter, filter.Server, identity)
	data.Tools = u.pickerOptions(ctx, "tool", filter, filter.Tool, identity)
	data.Workspaces = u.pickerOptions(ctx, "workspace", filter, filter.Workspace, func(value string) string { return labelWorkspace(value).Label })
	data.Sessions = u.pickerOptions(ctx, "session", filter, filter.Session, principal.Short)

	if session.Admin {
		data.Principals = u.pickerOptions(ctx, "principal", filter, filter.Principal, func(value string) string { return value })
		names := u.namesFor(ctx, session.Principal, optionValues(data.Principals)...)

		for index := range data.Principals {
			data.Principals[index].Label = names.show(data.Principals[index].Value).Text
		}
	}

	for _, category := range auditCategories {
		data.Categories = append(data.Categories, option{Value: category.Key, Label: category.Label, Selected: category.Key == filter.Category})
	}

	data.Categories = append([]option{{Value: "", Label: "Any category", Selected: filter.Category == ""}}, data.Categories...)

	for _, eventType := range knownAuditTypes() {
		data.EventTypes = append(data.EventTypes, option{
			Value: eventType, Label: auditTypeLabel(eventType), Selected: slices.Contains(filter.EventTypes, eventType),
		})
	}

	for _, outcome := range auditOutcomes {
		outcome.Selected = outcome.Value == filter.Outcome
		data.Outcomes = append(data.Outcomes, outcome)
	}

	for _, preset := range auditRangePresets {
		preset.Selected = preset.Value == filter.Range || (filter.Range == "" && preset.Value == "custom")
		data.Ranges = append(data.Ranges, preset)
	}

	u.render(writer, request, "audit", "Audit", data)
}

func optionValues(options []option) []string {
	var values []string

	for _, entry := range options {
		if entry.Value != "" {
			values = append(values, entry.Value)
		}
	}

	return values
}

// countParams is params without a page.
func (f auditFilter) countParams() db.CountAuditEventsParams {
	params := f.params()

	return db.CountAuditEventsParams{
		FromTime: params.FromTime, ToTime: params.ToTime, Principal: params.Principal, Workspace: params.Workspace,
		Session: params.Session, Server: params.Server, Tool: params.Tool, Types: params.Types, Outcome: params.Outcome,
	}
}

// exportPageSize is the rows read at a time by an export.
const exportPageSize = 500

// auditExport streams the events of a filter as JSON Lines, newest first.
func (u *UI) auditExport(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	filter, err := parseAuditFilter(request.URL.Query(), session, time.Now())
	if err != nil {
		http.Error(writer, "The filter is not valid: "+err.Error()+".", http.StatusBadRequest)

		return
	}

	writer.Header().Set("Content-Type", "application/x-ndjson")
	writer.Header().Set("Content-Disposition", `attachment; filename="audit.jsonl"`)

	encoder := json.NewEncoder(writer)
	params := filter.params()
	params.PageSize = exportPageSize

	for {
		rows, err := u.queries.ListAuditEventsPage(ctx, params)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot export audit events for the web UI", "error", err)

			return
		}

		for _, row := range rows {
			line := map[string]any{
				"id": row.ID, "time": row.OccurredAt.Time.UTC().Format(time.RFC3339Nano), "type": row.Type,
				"principal": row.Principal, "workspace": row.Workspace, "session": row.Session,
				"attestation": row.Attestation, "server": row.Server, "tool": row.Tool,
				"traceId": row.TraceID, "requestId": row.RequestID, "details": json.RawMessage(row.Details),
			}
			if encoder.Encode(line) != nil {
				return
			}
		}

		if len(rows) < exportPageSize {
			return
		}

		last := rows[len(rows)-1]
		params.BeforeAt = pgtype.Timestamptz{Time: last.OccurredAt.Time, Valid: true}
		params.BeforeID = last.ID
	}
}

// eventPage is the data of the event page.
type eventPage struct {
	auditEvent
	Summary     auditSummary
	Time        timeView
	SessionPage string
	Fields      []eventField
	Related     []auditRow
	PrettyBody  string
	Filter      auditFilter
}

// eventField is one line of the key and value list.
type eventField struct {
	Name, Value, Raw, URL string
}

// auditEventPage shows one event to its principal or an admin; anyone else
// gets 404, so the key leaks nothing.
func (u *UI) auditEventPage(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()
	now := time.Now()

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
	u.attachPayload(ctx, &event, occurredAt, id)

	event.Who = u.namesFor(ctx, session.Principal, event.Principal).show(event.Principal)
	filter := auditFilter{Range: "24h"}
	page := eventPage{
		auditEvent: event,
		Summary:    summarize(event),
		Time:       relativeTime(now, event.OccurredAt),
	}

	if event.Session != "" {
		page.SessionPage = u.sessionPages(ctx, session, []string{event.Session})[event.Session]
	}

	page.Fields = eventFields(page, filter)

	if event.Payload != "" && !event.PayloadTruncated {
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(event.Payload), "", "  ") == nil {
			page.PrettyBody = pretty.String()
		}
	}

	if event.RequestID != "" || event.TraceID != "" {
		related, err := u.queries.ListRelatedAuditEvents(ctx, db.ListRelatedAuditEventsParams{
			FromTime:  pgtype.Timestamptz{Time: occurredAt.Add(-24 * time.Hour), Valid: true},
			ToTime:    pgtype.Timestamptz{Time: occurredAt.Add(24 * time.Hour), Valid: true},
			Principal: map[bool]string{true: "", false: session.Principal}[session.Admin],
			RequestID: event.RequestID,
			TraceID:   event.TraceID,
		})
		if err == nil {
			rows := make([]db.ListAuditEventsPageRow, 0, len(related))
			for _, entry := range related {
				if entry.ID != id {
					rows = append(rows, db.ListAuditEventsPageRow(entry))
				}
			}

			page.Related = u.buildRows(ctx, rows, filter, session, now)
		}
	}

	u.render(writer, request, "audit_event", "Audit event", page)
}

func eventFields(page eventPage, filter auditFilter) []eventField {
	fields := []eventField{
		{Name: "Exact time", Value: page.OccurredAt.Format("2006-01-02 15:04:05.000 UTC")},
		{Name: "Event type", Value: auditTypeLabel(page.Type), Raw: page.Type, URL: filter.with("type", page.Type)},
	}

	add := func(name, value, raw, link string) {
		if value != "" {
			fields = append(fields, eventField{Name: name, Value: value, Raw: raw, URL: link})
		}
	}

	add("Session", principal.Short(page.Session), page.Session, filter.with("session", page.Session))
	add("Attestation", page.Attestation, "", "")
	add("Server", page.Server, "", filter.with("server", page.Server))
	add("Tool", page.Tool, "", filter.with("tool", page.Tool))
	add("Trace", principal.Short(page.TraceID), page.TraceID, "")
	add("Request", principal.Short(page.RequestID), page.RequestID, "")

	return fields
}
