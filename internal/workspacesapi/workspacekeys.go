package workspacesapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

// WorkspaceKeysPath is the route of the workspace key API.
const WorkspaceKeysPath = "/api/v1/workspace-keys"

// MaxKeyRegistrationBytes bounds the body of a key registration.
const MaxKeyRegistrationBytes = 4 << 10

// KeyService registers, lists and revokes workspace keys.
// *gateway.Workspaces implements it.
type KeyService interface {
	RegisterKey(ctx context.Context, identity auth.Identity, name, publicKey string) (gateway.KeyRegistration, error)
	WorkspaceKeys(ctx context.Context, identity auth.Identity) (gateway.WorkspaceKeyList, error)
	RevokeKey(ctx context.Context, identity auth.Identity, name, via string) (gateway.KeyRevocation, error)
}

// KeyRegistration is the body of `POST /api/v1/workspace-keys`.
type KeyRegistration struct {
	Name string `json:"name" pattern:"^[A-Za-z0-9._-]{1,63}$"`
	// PublicKey is an Ed25519 public key of 32 bytes, base64url without
	// padding.
	PublicKey string `json:"publicKey"`
}

// KeyActive answers a registration of a key that is active for the name
// already.
type KeyActive struct {
	Status      string `json:"status" enum:"active"`
	Name        string `json:"name"`
	Workspace   string `json:"workspace"`
	Fingerprint string `json:"fingerprint"`
}

// KeyRequested answers a registration with its pending key request and the
// page to approve it at.
type KeyRequested struct {
	Request     string `json:"request"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	Workspace   string `json:"workspace"`
	Fingerprint string `json:"fingerprint"`
	// Replaces is the fingerprint of the active key with the same name, or
	// null.
	Replaces *string `json:"replaces"`
}

// WorkspaceKey is one active key or pending key request.
type WorkspaceKey struct {
	Name         string    `json:"name"`
	Workspace    string    `json:"workspace"`
	Fingerprint  string    `json:"fingerprint"`
	Status       string    `json:"status" enum:"active,pending"`
	RegisteredAt time.Time `json:"registeredAt"`
	// ApprovedAt is null for a pending request.
	ApprovedAt *time.Time `json:"approvedAt"`
	// LastUsedAt is null until the key attests a request.
	LastUsedAt *time.Time `json:"lastUsedAt"`
	// Request is the pending request's ID, or null for an active key.
	Request *string `json:"request"`
}

// WorkspaceKeyList is the body of `GET /api/v1/workspace-keys`.
type WorkspaceKeyList struct {
	// Enabled is false when workspace keys are off on this gateway: the
	// keys don't attest.
	Enabled bool           `json:"enabled"`
	Keys    []WorkspaceKey `json:"keys"`
}

// KeyRevoked is the body of `DELETE /api/v1/workspace-keys/{name}`.
type KeyRevoked struct {
	// Revoked is true when an active key was revoked.
	Revoked bool `json:"revoked"`
	// DeniedRequests is the number of pending key requests for the name
	// that were denied.
	DeniedRequests int `json:"deniedRequests"`
}

// KeyName is the path of `DELETE /api/v1/workspace-keys/{name}`.
type KeyName struct {
	Name string `path:"name"`
}

// KeyOperations describes the workspace key routes.
var KeyOperations = []apidoc.Operation{
	{
		Method:  http.MethodPost,
		Path:    WorkspaceKeysPath,
		Summary: "Register a workspace key; it becomes a key access request that the user approves in the web UI",
		Request: KeyRegistration{},
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: KeyActive{}}, {Status: http.StatusAccepted, Body: KeyRequested{}}},
			textErrors(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict,
				http.StatusRequestEntityTooLarge, http.StatusInternalServerError)...),
	},
	{
		Method:  http.MethodGet,
		Path:    WorkspaceKeysPath,
		Summary: "List the caller's active workspace keys and pending key requests",
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: WorkspaceKeyList{}}},
			textErrors(http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError)...),
	},
	{
		Method:  http.MethodDelete,
		Path:    WorkspaceKeysPath + "/{name}",
		Summary: "Revoke the caller's workspace key and deny its pending key requests",
		Request: KeyName{},
		Responses: append([]apidoc.Response{{Status: http.StatusOK, Body: KeyRevoked{}}},
			textErrors(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError)...),
	},
}

func (h *Handler) registerKeyRoutes(mux Mux, authenticate func(http.Handler) http.Handler) {
	if h.Keys == nil {
		return
	}

	mux.Handle("POST "+WorkspaceKeysPath, authenticate(http.HandlerFunc(h.registerKey)))
	mux.Handle("GET "+WorkspaceKeysPath, authenticate(http.HandlerFunc(h.listKeys)))
	mux.Handle("DELETE "+WorkspaceKeysPath+"/{name}", authenticate(http.HandlerFunc(h.revokeKey)))
}

func (h *Handler) registerKey(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, messageSignIn, http.StatusUnauthorized)

		return
	}

	var body KeyRegistration

	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, MaxKeyRegistrationBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(writer, "The registration is too large.", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(writer, "The registration isn't valid JSON.", http.StatusBadRequest)

		return
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		http.Error(writer, "The registration isn't valid JSON.", http.StatusBadRequest)

		return
	}

	result, err := h.Keys.RegisterKey(request.Context(), identity, body.Name, body.PublicKey)

	var refusal *gateway.WorkspaceError

	switch {
	case errors.As(err, &refusal):
		http.Error(writer, refusal.Message, refusal.Status)
	case err != nil:
		h.internal(writer, request, "registering a workspace key failed", err)
	case result.Active:
		writeJSON(writer, http.StatusOK, KeyActive{
			Status: gateway.KeyStatusActive, Name: result.Name, Workspace: result.Workspace, Fingerprint: result.Fingerprint,
		})
	default:
		answer := KeyRequested{
			Request: result.Request, URL: h.ApprovalURL(result.Request),
			Name: result.Name, Workspace: result.Workspace, Fingerprint: result.Fingerprint,
		}
		if result.Replaces != "" {
			answer.Replaces = &result.Replaces
		}

		writeJSON(writer, http.StatusAccepted, answer)
	}
}

func (h *Handler) listKeys(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, messageSignIn, http.StatusUnauthorized)

		return
	}

	list, err := h.Keys.WorkspaceKeys(request.Context(), identity)

	var refusal *gateway.WorkspaceError

	switch {
	case errors.As(err, &refusal):
		http.Error(writer, refusal.Message, refusal.Status)
	case err != nil:
		h.internal(writer, request, "listing workspace keys failed", err)
	default:
		body := WorkspaceKeyList{Enabled: list.Enabled, Keys: make([]WorkspaceKey, 0, len(list.Keys))}

		for _, key := range list.Keys {
			entry := WorkspaceKey{
				Name: key.Name, Workspace: key.Workspace, Fingerprint: key.Fingerprint, Status: key.Status,
				RegisteredAt: key.RegisteredAt, ApprovedAt: key.ApprovedAt, LastUsedAt: key.LastUsedAt,
			}
			if key.Request != "" {
				entry.Request = &key.Request
			}

			body.Keys = append(body.Keys, entry)
		}

		writeJSON(writer, http.StatusOK, body)
	}
}

func (h *Handler) revokeKey(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, messageSignIn, http.StatusUnauthorized)

		return
	}

	result, err := h.Keys.RevokeKey(request.Context(), identity, request.PathValue("name"), gateway.ViaCLI)

	var refusal *gateway.WorkspaceError

	switch {
	case errors.As(err, &refusal):
		http.Error(writer, refusal.Message, refusal.Status)
	case err != nil:
		h.internal(writer, request, "revoking a workspace key failed", err)
	default:
		writeJSON(writer, http.StatusOK, KeyRevoked{Revoked: result.Revoked, DeniedRequests: result.DeniedRequests})
	}
}
