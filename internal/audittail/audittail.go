// Package audittail serves `GET /api/v1/audit/tail`: the audit events the
// caller may see, as an NDJSON stream that follows new events
// (docs/design/phase-3.md §5).
package audittail

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Path is the route `toolgate audit tail` calls.
const Path = "/api/v1/audit/tail"

// ContentType is the media type of the stream.
const ContentType = "application/x-ndjson"

// Bounds of a tail (docs/design/phase-3.md §9).
const (
	// BacklogEvents is how many past events a tail without `after` starts
	// with, and BacklogAge how far back it looks for them.
	BacklogEvents = 20
	BacklogAge    = 24 * time.Hour
	// MaxAfterAge is how far back `after` can start; an older key starts
	// there.
	MaxAfterAge = 31 * 24 * time.Hour
	// MaxRowsPerQuery is the most rows one query reads.
	MaxRowsPerQuery = 500
	// MaxPerPrincipal and MaxTotal bound the open tails on one replica.
	MaxPerPrincipal = 4
	MaxTotal        = 64
	// DefaultPollInterval, DefaultKeepalive and DefaultMaxDuration are the
	// poll period, the idle time before a keepalive line, and the longest
	// stream.
	DefaultPollInterval = 2 * time.Second
	DefaultKeepalive    = 15 * time.Second
	DefaultMaxDuration  = time.Hour
	// writeTimeout bounds each write to the client.
	writeTimeout = 30 * time.Second
)

// TooManyMessage is the body of the 429 answer.
const TooManyMessage = "Too many audit tails are open. Close one first."

// Event is one line of the stream. Payloads are never streamed.
type Event struct {
	// Key is `<occurred_at in Unix microseconds>-<id>`; pass the last one
	// as `after` to continue.
	Key         string          `json:"key"`
	OccurredAt  time.Time       `json:"occurredAt"`
	Type        string          `json:"type"`
	Principal   string          `json:"principal"`
	Workspace   string          `json:"workspace"`
	Session     string          `json:"session"`
	Attestation string          `json:"attestation"`
	Server      string          `json:"server"`
	Tool        string          `json:"tool"`
	TraceID     string          `json:"traceId"`
	RequestID   string          `json:"requestId"`
	Details     json.RawMessage `json:"details"`
}

// Request is the request type of the route.
type Request struct {
	After string `query:"after" description:"Continue after this event key (the key of the last event received)."`
}

// Operations describes the API's routes.
var Operations = []apidoc.Operation{{
	Method:  http.MethodGet,
	Path:    Path,
	Summary: "Stream the audit events the caller may see, as NDJSON, following new ones",
	Request: Request{},
	Responses: []apidoc.Response{
		{Status: http.StatusOK, Body: Event{}, ContentType: ContentType},
		{Status: http.StatusBadRequest, Body: apidoc.TextError(""), ContentType: "text/plain"},
		{Status: http.StatusUnauthorized, Body: apidoc.TextError(""), ContentType: "text/plain"},
		{Status: http.StatusTooManyRequests, Body: apidoc.TextError(""), ContentType: "text/plain"},
		{Status: http.StatusInternalServerError, Body: apidoc.TextError(""), ContentType: "text/plain"},
	},
}}

// FormatKey returns the key of the event with this primary key.
func FormatKey(occurredAt time.Time, id int64) string {
	return strconv.FormatInt(occurredAt.UnixMicro(), 10) + "-" + strconv.FormatInt(id, 10)
}

// ParseKey reads `<Unix microseconds>-<id>`.
func ParseKey(key string) (time.Time, int64, bool) {
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

// Querier reads the audit events. *db.Queries implements it.
type Querier interface {
	ListAuditTail(ctx context.Context, arg db.ListAuditTailParams) ([]db.ListAuditTailRow, error)
	ListAuditTailBacklog(ctx context.Context, arg db.ListAuditTailBacklogParams) ([]db.ListAuditTailBacklogRow, error)
}

// Options configure a Handler. Zero durations take the defaults.
type Options struct {
	Queries Querier
	// AdminGroup names the admins: users whose token groups hold it see
	// every principal's events. Empty means nobody is an admin.
	AdminGroup   string
	Log          *slog.Logger
	PollInterval time.Duration
	Keepalive    time.Duration
	MaxDuration  time.Duration
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
}

// Handler serves the route for the identity auth.Middleware put in the
// request context. Create it with New.
type Handler struct {
	options Options

	shutdown     context.Context
	stopAll      context.CancelFunc
	mu           sync.Mutex
	open         int
	perPrincipal map[string]int
}

// New returns a Handler.
func New(options Options) *Handler {
	if options.PollInterval <= 0 {
		options.PollInterval = DefaultPollInterval
	}

	if options.Keepalive <= 0 {
		options.Keepalive = DefaultKeepalive
	}

	if options.MaxDuration <= 0 {
		options.MaxDuration = DefaultMaxDuration
	}

	if options.Now == nil {
		options.Now = time.Now
	}

	shutdown, stopAll := context.WithCancel(context.Background())

	return &Handler{options: options, shutdown: shutdown, stopAll: stopAll, perPrincipal: map[string]int{}}
}

// Shutdown ends every open stream cleanly, and later ones at once. Register
// it with http.Server.RegisterOnShutdown so open tails don't hold up a
// shutdown.
func (h *Handler) Shutdown() {
	h.stopAll()
}

// Mux is where Register adds the route: an *http.ServeMux or the gateway.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Register adds the route to mux, wrapped by authenticate.
func (h *Handler) Register(mux Mux, authenticate func(http.Handler) http.Handler) {
	mux.Handle("GET "+Path, authenticate(h))
}

// IsAdmin reports whether identity may see every principal's events: a
// user, at any attestation, whose token groups hold adminGroup. Service
// accounts and SPIFFE workloads never are.
func IsAdmin(identity auth.Identity, adminGroup string) bool {
	return adminGroup != "" && identity.Kind == auth.KindUser && slices.Contains(identity.Groups, adminGroup)
}

// errStreamEnd ends a stream because the client is gone.
var errStreamEnd = errors.New("client gone")

// ServeHTTP streams the events.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok || identity.Principal == "" {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	principal := identity.Principal
	if IsAdmin(identity, h.options.AdminGroup) {
		principal = ""
	}

	now := h.options.Now()

	var (
		cursor    cursor
		hasCursor bool
	)

	if after := request.URL.Query().Get("after"); after != "" {
		at, id, valid := ParseKey(after)
		if !valid {
			http.Error(writer, "The after parameter must be an event key: <Unix microseconds>-<id>.", http.StatusBadRequest)

			return
		}

		cursor, hasCursor = startAfter(at, id, now), true
	}

	if !h.acquire(identity.Principal) {
		http.Error(writer, TooManyMessage, http.StatusTooManyRequests)

		return
	}
	defer h.release(identity.Principal)

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()

	stop := context.AfterFunc(h.shutdown, cancel)
	defer stop()

	end := now.Add(h.options.MaxDuration)
	if !identity.Expiry.IsZero() && identity.Expiry.Before(end) {
		end = identity.Expiry
	}

	ctx, cancelEnd := context.WithDeadline(ctx, end)
	defer cancelEnd()

	stream := &stream{writer: writer, controller: http.NewResponseController(writer), lastWrite: now, now: h.options.Now}

	writer.Header().Set("Content-Type", ContentType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)

	if err := stream.flush(); err != nil {
		return
	}

	if !hasCursor {
		var err error

		cursor, err = h.backlog(ctx, stream, principal, now)
		if err != nil {
			h.logFailure(ctx, err)

			return
		}
	}

	h.follow(ctx, stream, principal, cursor)
}

// cursor is the key of the last event sent.
type cursor struct {
	at time.Time
	id int64
}

// startAfter returns the cursor for an `after` key: the key itself, or
// MaxAfterAge back when the key is older. A key older than retention then
// starts at the oldest remaining row, because the query reads every row
// after the cursor.
func startAfter(at time.Time, id int64, now time.Time) cursor {
	if oldest := now.Add(-MaxAfterAge); at.Before(oldest) {
		return cursor{at: oldest}
	}

	return cursor{at: at, id: id}
}

// backlog sends the newest BacklogEvents events of the past BacklogAge and
// returns the cursor after them.
func (h *Handler) backlog(ctx context.Context, stream *stream, principal string, now time.Time) (cursor, error) {
	since := now.Add(-BacklogAge)

	rows, err := h.options.Queries.ListAuditTailBacklog(ctx, db.ListAuditTailBacklogParams{
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
		Principal: principal,
		MaxRows:   BacklogEvents,
	})
	if err != nil {
		return cursor{}, err
	}

	// Without past events, every later row of the past BacklogAge is new.
	if len(rows) == 0 {
		return cursor{at: since}, nil
	}

	ordered := make([]db.ListAuditTailRow, 0, len(rows))
	for index := len(rows) - 1; index >= 0; index-- {
		ordered = append(ordered, db.ListAuditTailRow(rows[index]))
	}

	return stream.send(ordered)
}

// follow polls for events after the cursor until ctx ends.
func (h *Handler) follow(ctx context.Context, stream *stream, principal string, current cursor) {
	ticker := time.NewTicker(h.options.PollInterval)
	defer ticker.Stop()

	for {
		for {
			rows, err := h.options.Queries.ListAuditTail(ctx, db.ListAuditTailParams{
				AfterAt:   pgtype.Timestamptz{Time: current.at, Valid: true},
				AfterID:   current.id,
				Principal: principal,
				MaxRows:   MaxRowsPerQuery,
			})
			if err != nil {
				if ctx.Err() == nil {
					h.logFailure(ctx, err)
				}

				return
			}

			if len(rows) > 0 {
				current, err = stream.send(rows)
				if err != nil {
					return
				}
			}

			// A full page may have more behind it: query again at once.
			if len(rows) < MaxRowsPerQuery {
				break
			}
		}

		if stream.now().Sub(stream.lastWrite) >= h.options.Keepalive {
			if err := stream.write([]byte("\n")); err != nil {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) logFailure(ctx context.Context, err error) {
	if h.options.Log != nil {
		h.options.Log.ErrorContext(ctx, "audit tail query failed", "error", err)
	}
}

// acquire counts a new tail for principal, or reports that a limit is
// reached.
func (h *Handler) acquire(principal string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.open >= MaxTotal || h.perPrincipal[principal] >= MaxPerPrincipal {
		return false
	}

	h.open++
	h.perPrincipal[principal]++

	return true
}

func (h *Handler) release(principal string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.open--

	h.perPrincipal[principal]--
	if h.perPrincipal[principal] == 0 {
		delete(h.perPrincipal, principal)
	}
}

// stream writes NDJSON lines to one client.
type stream struct {
	writer     http.ResponseWriter
	controller *http.ResponseController
	lastWrite  time.Time
	now        func() time.Time
}

// send writes rows as one batch and returns the cursor after the last one.
func (s *stream) send(rows []db.ListAuditTailRow) (cursor, error) {
	var batch []byte

	for _, row := range rows {
		line, err := json.Marshal(eventOf(row))
		if err != nil {
			return cursor{}, err
		}

		batch = append(batch, line...)
		batch = append(batch, '\n')
	}

	if err := s.write(batch); err != nil {
		return cursor{}, err
	}

	last := rows[len(rows)-1]

	return cursor{at: last.OccurredAt.Time, id: last.ID}, nil
}

// write sends data and flushes it, extending the write deadline first.
func (s *stream) write(data []byte) error {
	_ = s.controller.SetWriteDeadline(s.now().Add(writeTimeout))

	if _, err := s.writer.Write(data); err != nil {
		return errStreamEnd
	}

	s.lastWrite = s.now()

	return s.flush()
}

func (s *stream) flush() error {
	if err := s.controller.Flush(); err != nil {
		return errStreamEnd
	}

	return nil
}

func eventOf(row db.ListAuditTailRow) Event {
	details := json.RawMessage(row.Details)
	if len(details) == 0 {
		details = json.RawMessage("{}")
	}

	return Event{
		Key:         FormatKey(row.OccurredAt.Time, row.ID),
		OccurredAt:  row.OccurredAt.Time.UTC(),
		Type:        row.Type,
		Principal:   row.Principal,
		Workspace:   row.Workspace,
		Session:     row.Session,
		Attestation: row.Attestation,
		Server:      row.Server,
		Tool:        row.Tool,
		TraceID:     row.TraceID,
		RequestID:   row.RequestID,
		Details:     details,
	}
}
