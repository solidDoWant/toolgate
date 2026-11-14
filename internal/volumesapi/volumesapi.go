// Package volumesapi serves the admin routes for persistent volumes:
// `GET /api/v1/admin/volumes` lists the volume rows and
// `DELETE /api/v1/admin/volumes/{id}` marks one for deletion. The routes
// read and mark rows only; the leader deletes the PersistentVolume.
package volumesapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Path is the collection route; a volume is at Path + "/" + its ID.
const Path = "/api/v1/admin/volumes"

const (
	// PageSize is the most rows one list response holds.
	PageSize = 200
	// MaxOwnerLength bounds the owner filter, in bytes.
	MaxOwnerLength = 512
	// deleteReason is the reason an admin's deletion records.
	deleteReason = "admin"
)

// idPattern is the form of a volume row ID.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Volume is one row of the volume table.
type Volume struct {
	ID                string `json:"id"`
	Server            string `json:"server"`
	TemplateNamespace string `json:"templateNamespace"`
	Volume            string `json:"volume"`
	Scope             string `json:"scope"`
	// Owner is empty for a per-template volume.
	Owner        string `json:"owner"`
	StorageClass string `json:"storageClass"`
	SizeBytes    int64  `json:"sizeBytes"`
	// PersistentVolume is null until the first PV is recorded.
	PersistentVolume *string `json:"persistentVolume"`
	// InUseBy is the instance that holds the volume, or null.
	InUseBy      *string   `json:"inUseBy"`
	CreatedAt    time.Time `json:"createdAt"`
	LastUsedAt   time.Time `json:"lastUsedAt"`
	DeleteReason string    `json:"deleteReason"`
	Problem      string    `json:"problem"`
}

// List is the body of `GET /api/v1/admin/volumes`.
type List struct {
	Volumes []Volume `json:"volumes"`
	// Next is the ID to pass as `after` for the next page. It is absent on
	// the last page.
	Next string `json:"next,omitempty"`
}

// ListRequest is the request type of the list route.
type ListRequest struct {
	Owner string `query:"owner" description:"Only the per-user volumes of this principal."`
	After string `query:"after" description:"Continue after this volume ID (the next value of the previous page)."`
}

// DeleteRequest is the request type of the delete route.
type DeleteRequest struct {
	ID string `path:"id" description:"The volume ID."`
}

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
		Path:    Path,
		Summary: "List the persistent volumes (admins only)",
		Request: ListRequest{},
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: List{}}},
			textErrors(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError)...),
	},
	{
		Method:  http.MethodDelete,
		Path:    Path + "/{id}",
		Summary: "Mark a persistent volume for deletion (admins only); the leader deletes it",
		Request: DeleteRequest{},
		Responses: append([]apidoc.Response{{Status: http.StatusAccepted, Body: Volume{}}},
			textErrors(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError)...),
	},
}

// Querier reads the volume rows. *db.Queries implements it.
type Querier interface {
	ListPersistentVolumes(ctx context.Context, arg db.ListPersistentVolumesParams) ([]db.PersistentVolume, error)
}

// Beginner starts the transaction of a deletion. *pgxpool.Pool implements it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Handler serves the routes for the identity auth.Middleware put in the
// request context.
type Handler struct {
	Queries Querier
	Pool    Beginner
	// AdminGroup is the gateway's adminGroup. Without one, every caller is
	// refused.
	AdminGroup string
	// Output receives the audit event lines.
	Output io.Writer
	Log    *slog.Logger
}

// Mux is where Register adds the routes: an *http.ServeMux or the gateway.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Register adds the routes to mux, each wrapped by authenticate.
func (h *Handler) Register(mux Mux, authenticate func(http.Handler) http.Handler) {
	mux.Handle("GET "+Path, authenticate(http.HandlerFunc(h.list)))
	mux.Handle("DELETE "+Path+"/{id}", authenticate(http.HandlerFunc(h.remove)))
}

// admin returns the caller's identity when it is an admin, and otherwise
// answers 401 or 403.
func (h *Handler) admin(writer http.ResponseWriter, request *http.Request) (auth.Identity, bool) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return auth.Identity{}, false
	}

	if !audittail.IsAdmin(identity, h.AdminGroup) {
		http.Error(writer, "Only an admin may manage volumes.", http.StatusForbidden)

		return auth.Identity{}, false
	}

	return identity, true
}

func (h *Handler) list(writer http.ResponseWriter, request *http.Request) {
	if _, ok := h.admin(writer, request); !ok {
		return
	}

	owner := request.URL.Query().Get("owner")
	after := request.URL.Query().Get("after")

	if len(owner) > MaxOwnerLength {
		http.Error(writer, "The owner filter is too long.", http.StatusBadRequest)

		return
	}

	if after != "" && !idPattern.MatchString(after) {
		http.Error(writer, "The after value is not a volume ID.", http.StatusBadRequest)

		return
	}

	// One more than the page tells whether there is a next page.
	rows, err := h.Queries.ListPersistentVolumes(request.Context(), db.ListPersistentVolumesParams{
		AfterID: after, Owner: owner, PageSize: PageSize + 1,
	})
	if err != nil {
		h.internal(request, writer, "listing volumes failed", err)

		return
	}

	body := List{Volumes: []Volume{}}

	for index, row := range rows {
		if index == PageSize {
			body.Next = rows[PageSize-1].ID

			break
		}

		body.Volumes = append(body.Volumes, toVolume(row))
	}

	writeJSON(writer, http.StatusOK, body)
}

func (h *Handler) remove(writer http.ResponseWriter, request *http.Request) {
	identity, ok := h.admin(writer, request)
	if !ok {
		return
	}

	id := request.PathValue("id")
	if !idPattern.MatchString(id) {
		http.Error(writer, "No such volume.", http.StatusNotFound)

		return
	}

	row, found, err := h.mark(request.Context(), identity, id)
	if err != nil {
		h.internal(request, writer, "marking a volume for deletion failed", err)

		return
	}

	if !found {
		http.Error(writer, "No such volume.", http.StatusNotFound)

		return
	}

	writeJSON(writer, http.StatusAccepted, toVolume(row))
}

// mark sets the admin deletion mark and writes the audit event in one
// transaction. A row that is already being deleted is returned unchanged.
func (h *Handler) mark(ctx context.Context, identity auth.Identity, id string) (db.PersistentVolume, bool, error) {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return db.PersistentVolume{}, false, err
	}

	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)

	changed, err := queries.MarkPersistentVolumeForDeletion(ctx, db.MarkPersistentVolumeForDeletionParams{
		ID: id, DeleteReason: deleteReason, AnyHolder: true,
	})
	if err != nil {
		return db.PersistentVolume{}, false, err
	}

	row, err := queries.GetPersistentVolume(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PersistentVolume{}, false, nil
	}

	if err != nil {
		return db.PersistentVolume{}, false, err
	}

	if changed > 0 {
		err = audit.NewRecorder(h.Output, queries).Record(ctx, audit.Event{
			Type:        audit.TypeVolumeDelete,
			Principal:   identity.Principal,
			Attestation: identity.Attestation,
			Server:      row.TemplateName,
			Details: map[string]any{
				"volume":           row.VolumeName,
				"persistentVolume": row.PvName.String,
				"reason":           deleteReason,
				"owner":            row.ScopeID,
			},
		})
		if err != nil {
			return db.PersistentVolume{}, false, err
		}
	}

	return row, true, tx.Commit(ctx)
}

func (h *Handler) internal(request *http.Request, writer http.ResponseWriter, message string, err error) {
	if h.Log != nil {
		h.Log.ErrorContext(request.Context(), message, "error", err)
	}

	http.Error(writer, "The gateway could not complete the request. Try again later.", http.StatusInternalServerError)
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func optional(text pgtype.Text) *string {
	if !text.Valid {
		return nil
	}

	return &text.String
}

func toVolume(row db.PersistentVolume) Volume {
	return Volume{
		ID:                row.ID,
		Server:            row.TemplateName,
		TemplateNamespace: row.TemplateNs,
		Volume:            row.VolumeName,
		Scope:             row.Scope,
		Owner:             row.ScopeID,
		StorageClass:      row.StorageClass,
		SizeBytes:         row.SizeBytes,
		PersistentVolume:  optional(row.PvName),
		InUseBy:           optional(row.InstanceID),
		CreatedAt:         row.CreatedAt.Time.UTC(),
		LastUsedAt:        row.LastUsedAt.Time.UTC(),
		DeleteReason:      row.DeleteReason,
		Problem:           row.Problem,
	}
}
