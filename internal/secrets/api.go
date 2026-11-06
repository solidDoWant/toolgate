package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
)

// Recorder writes audit events. *audit.Recorder implements it.
type Recorder interface {
	Record(ctx context.Context, event audit.Event) error
}

// SecretInfo is a secret as the API returns it: its name and dates, never
// its value.
type SecretInfo struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SecretList is the body of `GET /api/v1/secrets`.
type SecretList struct {
	Secrets []SecretInfo `json:"secrets"`
}

// SecretName is the request type of the operations that name one secret.
type SecretName struct {
	Name string `path:"name" pattern:"^[A-Za-z0-9._-]{1,128}$"`
}

// Operations describes the API's routes.
var Operations = []apidoc.Operation{
	{
		Method: http.MethodPut, Path: "/api/v1/secrets/{name}", Summary: "Store the request body as a secret's value",
		Request: SecretName{},
		Responses: []apidoc.Response{
			{Status: http.StatusOK, Body: SecretInfo{}},
			{Status: http.StatusBadRequest, Body: apidoc.TextError(""), ContentType: "text/plain"},
			{Status: http.StatusUnauthorized, Body: apidoc.TextError(""), ContentType: "text/plain"},
			{Status: http.StatusRequestEntityTooLarge, Body: apidoc.TextError(""), ContentType: "text/plain"},
		},
	},
	{
		Method: http.MethodGet, Path: "/api/v1/secrets", Summary: "List the caller's secrets: names and dates only",
		Responses: []apidoc.Response{
			{Status: http.StatusOK, Body: SecretList{}},
			{Status: http.StatusUnauthorized, Body: apidoc.TextError(""), ContentType: "text/plain"},
		},
	},
	{
		Method: http.MethodDelete, Path: "/api/v1/secrets/{name}", Summary: "Delete one of the caller's secrets",
		Request: SecretName{},
		Responses: []apidoc.Response{
			{Status: http.StatusNoContent},
			{Status: http.StatusNotFound, Body: apidoc.TextError(""), ContentType: "text/plain"},
			{Status: http.StatusUnauthorized, Body: apidoc.TextError(""), ContentType: "text/plain"},
		},
	},
}

// API serves `/api/v1/secrets` for the identity auth.Middleware put in the
// request context:
//
//   - `PUT /api/v1/secrets/{name}` stores the request body as the value;
//   - `GET /api/v1/secrets` lists names and dates;
//   - `DELETE /api/v1/secrets/{name}` deletes the secret.
//
// Each request acts only on the verified principal's own secrets.
type API struct {
	broker   *Broker
	recorder Recorder
	logger   *slog.Logger
}

// NewAPI returns the secrets API.
func NewAPI(broker *Broker, recorder Recorder, logger *slog.Logger) *API {
	return &API{broker: broker, recorder: recorder, logger: logger}
}

// Mux is where Register adds routes: an *http.ServeMux or the gateway.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Register adds the API's routes to mux, each wrapped by authenticate.
func (a *API) Register(mux Mux, authenticate func(http.Handler) http.Handler) {
	mux.Handle("PUT /api/v1/secrets/{name}", authenticate(http.HandlerFunc(a.set)))
	mux.Handle("GET /api/v1/secrets", authenticate(http.HandlerFunc(a.list)))
	mux.Handle("DELETE /api/v1/secrets/{name}", authenticate(http.HandlerFunc(a.delete)))
}

func (a *API) set(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	name := request.PathValue("name")
	if !ValidName(name) {
		http.Error(writer, "The secret name is invalid. It must match ^[A-Za-z0-9._-]{1,128}$.", http.StatusBadRequest)

		return
	}

	value, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, MaxValueBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(writer, ErrValueTooLong.Error()+".", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(writer, "The request body could not be read.", http.StatusBadRequest)

		return
	}

	info, err := a.broker.Set(request.Context(), identity.Principal, name, value)
	if err != nil {
		a.fail(writer, request, "storing a secret failed", err)

		return
	}

	if !a.record(writer, request, audit.TypeSecretSet, identity, name) {
		return
	}

	writeJSON(writer, http.StatusOK, toSecretInfo(info))
}

func (a *API) list(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	infos, err := a.broker.List(request.Context(), identity.Principal)
	if err != nil {
		a.fail(writer, request, "listing secrets failed", err)

		return
	}

	body := SecretList{Secrets: make([]SecretInfo, 0, len(infos))}
	for _, info := range infos {
		body.Secrets = append(body.Secrets, toSecretInfo(info))
	}

	writeJSON(writer, http.StatusOK, body)
}

func (a *API) delete(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	name := request.PathValue("name")

	err := a.broker.Delete(request.Context(), identity.Principal, name)
	if errors.Is(err, ErrNotFound) {
		http.Error(writer, "You have no secret with this name.", http.StatusNotFound)

		return
	}

	if err != nil {
		a.fail(writer, request, "deleting a secret failed", err)

		return
	}

	if !a.record(writer, request, audit.TypeSecretDelete, identity, name) {
		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

// record writes the audit event for a change. When that fails it answers
// 500 and returns false.
func (a *API) record(writer http.ResponseWriter, request *http.Request, eventType string, identity auth.Identity, name string) bool {
	err := a.recorder.Record(request.Context(), audit.Event{
		Type:        eventType,
		Principal:   identity.Principal,
		Workspace:   identity.Workspace,
		Session:     identity.Session,
		Attestation: identity.Attestation,
		Details:     map[string]any{"secret": name},
	})
	if err != nil {
		a.fail(writer, request, "writing the audit event failed", err)

		return false
	}

	return true
}

// fail logs err, which never holds a secret value, and answers 500.
func (a *API) fail(writer http.ResponseWriter, request *http.Request, message string, err error) {
	a.logger.ErrorContext(request.Context(), message, "error", err)
	http.Error(writer, "The gateway could not complete the request. Try again later.", http.StatusInternalServerError)
}

func toSecretInfo(info Info) SecretInfo {
	return SecretInfo{Name: info.Name, CreatedAt: info.CreatedAt.UTC(), UpdatedAt: info.UpdatedAt.UTC()}
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
