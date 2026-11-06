// Package workspacesapi serves the workspace API: `GET /api/v1/workspaces`,
// `POST /api/v1/workspaces/changes` and `GET /api/v1/access-requests/{id}`
// (docs/design/phase-3.md §10, "API and CLI").
package workspacesapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Routes of the API.
const (
	WorkspacesPath     = "/api/v1/workspaces"
	ChangesPath        = "/api/v1/workspaces/changes"
	AccessRequestsPath = "/api/v1/access-requests/"
)

// MaxChangeBytes bounds the body of a change.
const MaxChangeBytes = 64 << 10

// MessageScopingOff answers the workspace routes when the gateway has no
// web UI.
const MessageScopingOff = "Workspace scoping is off, because this gateway has no web UI."

const (
	messageSignIn   = "Sign in again: run `toolgate login`."
	messageInternal = "The gateway could not complete the request. Try again later."
)

// Defaults are the defaults for sessions with and without a verified
// workspace: none or all.
type Defaults struct {
	Verified   string `json:"verified"`
	Unverified string `json:"unverified"`
}

// Workspace is one workspace's list and scope entries.
type Workspace struct {
	Workspace string `json:"workspace"`
	// Servers is the workspace's list, or null when it has none: then its
	// default applies, narrowed by denied.
	Servers []string `json:"servers"`
	// Tools are the tools allowed one by one, as `<server>__<tool>` with the
	// tool's exposed name.
	Tools []string `json:"tools"`
	// Denied are the servers and `<server>__<tool>` tools denied in the
	// workspace, whatever the list and the default say.
	Denied    []string  `json:"denied"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// List is the body of `GET /api/v1/workspaces`.
type List struct {
	// Current is the request's verified workspace, or null.
	Current    *string     `json:"current"`
	Defaults   Defaults    `json:"defaults"`
	Workspaces []Workspace `json:"workspaces"`
}

// Change is the body of `POST /api/v1/workspaces/changes`.
type Change struct {
	Action string `json:"action" enum:"allow,deny,clear,defaults"`
	// Workspace defaults to the request's verified workspace.
	Workspace string `json:"workspace,omitempty"`
	// Session is one of the caller's open sessions that a deny applies to,
	// in place of workspace.
	Session string `json:"session,omitempty"`
	// Servers are server names. A deny also takes `<server>__<tool>` with
	// the tool's exposed name.
	Servers    []string `json:"servers,omitempty"`
	Verified   string   `json:"verified,omitempty" enum:"none,all"`
	Unverified string   `json:"unverified,omitempty" enum:"none,all"`
}

// Applied answers a narrowing change.
type Applied struct {
	Applied bool `json:"applied"`
}

// Requested answers a widening change: the pending access request and the
// page to approve it at.
type Requested struct {
	Request string `json:"request"`
	URL     string `json:"url"`
}

// AccessRequest is the body of `GET /api/v1/access-requests/{id}`.
type AccessRequest struct {
	ID        string    `json:"id"`
	Status    string    `json:"status" enum:"pending,approved,denied,expired"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AccessRequestID is the path of `GET /api/v1/access-requests/{id}`.
type AccessRequestID struct {
	ID string `path:"id" pattern:"^[0-9a-f]{32}$"`
}

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func textErrors(statuses ...int) []apidoc.Response {
	responses := make([]apidoc.Response, 0, len(statuses))
	for _, status := range statuses {
		responses = append(responses, apidoc.Response{Status: status, Body: apidoc.TextError(""), ContentType: "text/plain"})
	}

	return responses
}

// Operations describes the API's routes.
var Operations = []apidoc.Operation{
	{
		Method:  http.MethodGet,
		Path:    WorkspacesPath,
		Summary: "List the caller's workspace defaults and lists",
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: List{}}},
			textErrors(http.StatusUnauthorized, http.StatusConflict, http.StatusInternalServerError)...),
	},
	{
		Method:  http.MethodPost,
		Path:    ChangesPath,
		Summary: "Change the caller's workspace lists or defaults; a widening change becomes an access request",
		Request: Change{},
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: Applied{}}, {Status: http.StatusAccepted, Body: Requested{}}},
			textErrors(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
				http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusInternalServerError)...),
	},
	{
		Method:  http.MethodGet,
		Path:    AccessRequestsPath + "{id}",
		Summary: "Read one of the caller's access requests",
		Request: AccessRequestID{},
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: AccessRequest{}}},
			textErrors(http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError)...),
	},
}

// Service reads and changes workspace lists. *gateway.Workspaces
// implements it.
type Service interface {
	Scopes(ctx context.Context, principal string) (gateway.WorkspaceScopes, error)
	Request(ctx context.Context, identity auth.Identity, change gateway.WorkspaceChange) (gateway.WorkspaceResult, error)
	AccessRequest(ctx context.Context, principal, id string) (db.AccessRequest, error)
}

// Handler serves the routes for the identity auth.Middleware put in the
// request context.
type Handler struct {
	Workspaces Service
	// Enabled is false when the gateway has no web UI: the workspace routes
	// answer 409.
	Enabled bool
	// ApprovalURL returns the page of an access request.
	ApprovalURL func(id string) string
	Log         *slog.Logger
}

// Mux is where Register adds the routes: an *http.ServeMux or the gateway.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Register adds the routes to mux, each wrapped by authenticate.
func (h *Handler) Register(mux Mux, authenticate func(http.Handler) http.Handler) {
	mux.Handle("GET "+WorkspacesPath, authenticate(http.HandlerFunc(h.list)))
	mux.Handle("POST "+ChangesPath, authenticate(http.HandlerFunc(h.change)))
	mux.Handle("GET "+AccessRequestsPath+"{id}", authenticate(http.HandlerFunc(h.accessRequest)))
}

func (h *Handler) list(writer http.ResponseWriter, request *http.Request) {
	identity, ok := h.identity(writer, request)
	if !ok {
		return
	}

	scopes, err := h.Workspaces.Scopes(request.Context(), identity.Principal)
	if err != nil {
		h.internal(writer, request, "listing workspaces failed", err)

		return
	}

	body := List{
		Defaults:   Defaults{Verified: scopes.VerifiedDefault, Unverified: scopes.UnverifiedDefault},
		Workspaces: make([]Workspace, 0, len(scopes.Lists)),
	}

	if current := gateway.VerifiedWorkspace(identity); current != "" {
		body.Current = &current
	}

	listed := map[string]bool{}

	for _, list := range scopes.Lists {
		listed[list.Workspace] = true
		entries := scopes.Entries[list.Workspace]

		updated := list.UpdatedAt
		if entries.UpdatedAt.After(updated) {
			updated = entries.UpdatedAt
		}

		body.Workspaces = append(body.Workspaces, Workspace{
			Workspace: list.Workspace, Servers: list.Servers,
			Tools: nonNil(entries.Allowed), Denied: nonNil(entries.Denied), UpdatedAt: updated,
		})
	}

	unlisted := make([]string, 0, len(scopes.Entries))

	for workspace := range scopes.Entries {
		if !listed[workspace] {
			unlisted = append(unlisted, workspace)
		}
	}

	slices.Sort(unlisted)

	for _, workspace := range unlisted {
		entries := scopes.Entries[workspace]
		body.Workspaces = append(body.Workspaces, Workspace{
			Workspace: workspace, Tools: nonNil(entries.Allowed), Denied: nonNil(entries.Denied), UpdatedAt: entries.UpdatedAt,
		})
	}

	slices.SortFunc(body.Workspaces, func(a, b Workspace) int { return strings.Compare(a.Workspace, b.Workspace) })

	writeJSON(writer, http.StatusOK, body)
}

func (h *Handler) change(writer http.ResponseWriter, request *http.Request) {
	identity, ok := h.identity(writer, request)
	if !ok {
		return
	}

	var change Change

	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, MaxChangeBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&change); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(writer, "The change is too large.", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(writer, "The change isn't valid JSON.", http.StatusBadRequest)

		return
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		http.Error(writer, "The change isn't valid JSON.", http.StatusBadRequest)

		return
	}

	result, err := h.Workspaces.Request(request.Context(), identity, gateway.WorkspaceChange{
		Action: change.Action, Workspace: change.Workspace, Session: change.Session, Servers: change.Servers,
		Verified: change.Verified, Unverified: change.Unverified,
	})

	var refusal *gateway.WorkspaceError

	switch {
	case errors.As(err, &refusal):
		http.Error(writer, refusal.Message, refusal.Status)
	case err != nil:
		h.internal(writer, request, "changing a workspace failed", err)
	case result.Applied:
		writeJSON(writer, http.StatusOK, Applied{Applied: true})
	default:
		writeJSON(writer, http.StatusAccepted, Requested{Request: result.Request, URL: h.ApprovalURL(result.Request)})
	}
}

func (h *Handler) accessRequest(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, messageSignIn, http.StatusUnauthorized)

		return
	}

	id := request.PathValue("id")
	notFound := "Access request `" + id + "` doesn't exist."

	if !requestIDPattern.MatchString(id) {
		http.Error(writer, "Access request doesn't exist.", http.StatusNotFound)

		return
	}

	row, err := h.Workspaces.AccessRequest(request.Context(), identity.Principal, id)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		http.Error(writer, notFound, http.StatusNotFound)
	case err != nil:
		h.internal(writer, request, "reading an access request failed", err)
	default:
		writeJSON(writer, http.StatusOK, AccessRequest{ID: row.ID, Status: row.Status, ExpiresAt: row.ExpiresAt.Time.UTC()})
	}
}

// identity returns the caller, or answers 401 without one and 409 when
// scoping is off.
func (h *Handler) identity(writer http.ResponseWriter, request *http.Request) (auth.Identity, bool) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, messageSignIn, http.StatusUnauthorized)

		return auth.Identity{}, false
	}

	if !h.Enabled {
		http.Error(writer, MessageScopingOff, http.StatusConflict)

		return auth.Identity{}, false
	}

	return identity, true
}

func (h *Handler) internal(writer http.ResponseWriter, request *http.Request, message string, err error) {
	if h.Log != nil {
		h.Log.ErrorContext(request.Context(), message, "error", err)
	}

	http.Error(writer, messageInternal, http.StatusInternalServerError)
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
