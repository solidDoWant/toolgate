// Package sessionsapi serves `GET /api/v1/sessions`: the caller's open
// sessions and the servers each has enabled (docs/design/phase-3.md §5).
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

// Handler serves the route for the identity auth.Middleware put in the
// request context. It lists only that principal's sessions, for admins too.
type Handler struct {
	Queries Querier
	// IdleTimeout is the gateway's sessionIdleTimeout: a session idle for
	// longer is left out even before the cleanup pass ends it.
	IdleTimeout time.Duration
	Log         *slog.Logger
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
		if h.Log != nil {
			h.Log.ErrorContext(request.Context(), "listing sessions failed", "error", err)
		}

		http.Error(writer, "The gateway could not complete the request. Try again later.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(build(rows))
}

// build groups the rows, which arrive ordered by session, into the body.
func build(rows []db.ListOpenSessionsWithServersRow) List {
	body := List{Sessions: []Session{}}

	var lastID int64

	for _, row := range rows {
		if len(body.Sessions) == 0 || row.ID != lastID {
			if len(body.Sessions) == MaxSessions {
				body.Truncated = true

				break
			}

			lastID = row.ID
			body.Sessions = append(body.Sessions, Session{
				Session:     row.SessionID,
				Workspace:   row.Workspace,
				Attestation: row.Attestation,
				CreatedAt:   row.CreatedAt.Time.UTC(),
				LastSeenAt:  row.LastSeenAt.Time.UTC(),
				Servers:     []Server{},
			})
		}

		if row.ServerName.Valid {
			current := &body.Sessions[len(body.Sessions)-1]
			current.Servers = append(current.Servers, Server{Name: row.ServerName.String, EnabledAt: row.ServerEnabledAt.Time.UTC()})
		}
	}

	return body
}
