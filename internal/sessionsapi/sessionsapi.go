// Package sessionsapi serves `GET /api/v1/sessions`: the caller's open
// sessions, the servers each has enabled, and
// each one's allow and deny entries.
package sessionsapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Path is the route `toolgate sessions` calls.
const Path = "/api/v1/sessions"

// MaxSessions is the most sessions one response holds.
const MaxSessions = 200

// Server is one enabled server of a session.
type Server struct {
	Name      string    `json:"name"`
	EnabledAt time.Time `json:"enabledAt"`
}

// Session is one open session.
type Session struct {
	Session     string    `json:"session"`
	Workspace   string    `json:"workspace"`
	Attestation string    `json:"attestation"`
	CreatedAt   time.Time `json:"createdAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	Servers     []Server  `json:"servers"`
	// Allowed are the servers and `<server>__<tool>` tools, by exposed
	// name, allowed for this session only.
	Allowed []string `json:"allowed"`
	// Denied are the servers and tools denied in this session. They last
	// until the session is forgotten, so a resumed session keeps them.
	Denied []string `json:"denied"`
}

// List is the body of `GET /api/v1/sessions`.
type List struct {
	Sessions []Session `json:"sessions"`
	// Truncated is true when the caller has more open sessions than the
	// response holds.
	Truncated bool `json:"truncated"`
}

// Operations describes the API's routes.
var Operations = []apidoc.Operation{{
	Method:  http.MethodGet,
	Path:    Path,
	Summary: "List the caller's open sessions and their enabled servers",
	Responses: []apidoc.Response{
		{Status: http.StatusOK, Body: List{}},
		{Status: http.StatusUnauthorized, Body: apidoc.TextError(""), ContentType: "text/plain"},
		{Status: http.StatusInternalServerError, Body: apidoc.TextError(""), ContentType: "text/plain"},
	},
}}

// Querier reads the sessions. *db.Queries implements it.
type Querier interface {
	ListOpenSessionsWithServers(ctx context.Context, arg db.ListOpenSessionsWithServersParams) ([]db.ListOpenSessionsWithServersRow, error)
}

// EntryReader reads the scope entries of sessions by sessions row ID.
// *gateway.Workspaces implements it.
type EntryReader interface {
	SessionEntries(ctx context.Context, sessionPKs []int64) (map[int64]gateway.ScopeEntries, error)
}

// Handler serves the route for the identity auth.Middleware put in the
// request context. It lists only that principal's sessions, for admins too.
type Handler struct {
	Queries Querier
	// IdleTimeout is the gateway's sessionIdleTimeout: a session idle for
	// longer is left out even before the cleanup pass ends it.
	IdleTimeout time.Duration
	// Entries reads each session's allow and deny entries. Without it the
	// lists are empty.
	Entries EntryReader
	Log     *slog.Logger
}

// Mux is where Register adds the route: an *http.ServeMux or the gateway.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Register adds the route to mux, wrapped by authenticate.
func (h *Handler) Register(mux Mux, authenticate func(http.Handler) http.Handler) {
	mux.Handle("GET "+Path, authenticate(h))
}

// ServeHTTP writes the caller's open sessions as JSON.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	rows, err := h.Queries.ListOpenSessionsWithServers(request.Context(), db.ListOpenSessionsWithServersParams{
		Principal:   identity.Principal,
		IdleTimeout: pgtype.Interval{Microseconds: h.IdleTimeout.Microseconds(), Valid: true},
		// One more than the cap tells whether there were more.
		MaxRows: MaxSessions + 1,
	})
	if err != nil {
		h.internal(request, writer, err)

		return
	}

	body, ids := build(rows)

	if h.Entries != nil && len(ids) > 0 {
		entries, err := h.Entries.SessionEntries(request.Context(), ids)
		if err != nil {
			h.internal(request, writer, err)

			return
		}

		for index, id := range ids {
			if each, found := entries[id]; found {
				body.Sessions[index].Allowed, body.Sessions[index].Denied = each.Allowed, each.Denied
			}
		}
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(body)
}

func (h *Handler) internal(request *http.Request, writer http.ResponseWriter, err error) {
	if h.Log != nil {
		h.Log.ErrorContext(request.Context(), "listing sessions failed", "error", err)
	}

	http.Error(writer, "The gateway could not complete the request. Try again later.", http.StatusInternalServerError)
}

// build groups the rows, which arrive ordered by session, into the body,
// and returns the sessions row ID of each session in it.
func build(rows []db.ListOpenSessionsWithServersRow) (List, []int64) {
	body := List{Sessions: []Session{}}
	ids := []int64{}

	var lastID int64

	for _, row := range rows {
		if len(body.Sessions) == 0 || row.ID != lastID {
			if len(body.Sessions) == MaxSessions {
				body.Truncated = true

				break
			}

			lastID = row.ID
			ids = append(ids, row.ID)
			body.Sessions = append(body.Sessions, Session{
				Session:     row.SessionID,
				Workspace:   row.Workspace,
				Attestation: row.Attestation,
				CreatedAt:   row.CreatedAt.Time.UTC(),
				LastSeenAt:  row.LastSeenAt.Time.UTC(),
				Servers:     []Server{},
				Allowed:     []string{},
				Denied:      []string{},
			})
		}

		if row.ServerName.Valid {
			current := &body.Sessions[len(body.Sessions)-1]
			current.Servers = append(current.Servers, Server{Name: row.ServerName.String, EnabledAt: row.ServerEnabledAt.Time.UTC()})
		}
	}

	return body, ids
}
