package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// WorkspaceKeyRevokePath is the form that revokes one of the user's
// workspace keys.
const WorkspaceKeyRevokePath = WorkspacesPath + "/keys/revoke"

// shortFingerprintLength is how many characters of a key ID the Approvals
// list shows.
const shortFingerprintLength = 12

// shortFingerprint is the start of a key's fingerprint, for lists.
func shortFingerprint(keyID string) string {
	if len(keyID) > shortFingerprintLength {
		keyID = keyID[:shortFingerprintLength]
	}

	return gateway.KeyFingerprint(keyID) + "…"
}

// keyRequestView is what the approval page shows of a key request.
type keyRequestView struct {
	Name string
	// KeyWorkspace is the workspace the key gives, with the prefix
	// configured now.
	KeyWorkspace string
	Fingerprint  string
	// Replaces is the active key the request would replace, or nil.
	Replaces *keyReplaced
	// AskedFrom is the workspace the request came from; it is empty for a
	// request that named none.
	AskedFrom workspaceLabel
	Verified  bool
}

// keyReplaced is the key a key request would replace.
type keyReplaced struct {
	Fingerprint  string
	RegisteredAt time.Time
}

func (u *UI) keyRequestView(ctx context.Context, request db.AccessRequest) (keyRequestView, error) {
	var change gateway.KeyChange
	if err := json.Unmarshal(request.Change, &change); err != nil {
		return keyRequestView{}, fmt.Errorf("decode key request: %w", err)
	}

	view := keyRequestView{
		Name:         change.Name,
		KeyWorkspace: u.options.Workspaces.KeyWorkspace(change.Username, change.Name),
		Fingerprint:  gateway.KeyFingerprint(change.KeyID),
		Verified:     request.Verified,
	}

	if request.Workspace != "" {
		view.AskedFrom = labelWorkspace(request.Workspace)
	}

	current, err := u.queries.GetActiveWorkspaceKeyByName(ctx, db.GetActiveWorkspaceKeyByNameParams{Principal: request.Principal, Name: change.Name})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return keyRequestView{}, fmt.Errorf("read the key the request replaces: %w", err)
	default:
		view.Replaces = &keyReplaced{Fingerprint: gateway.KeyFingerprint(current.KeyID), RegisteredAt: current.RegisteredAt.Time}
	}

	return view, nil
}

// keyRow is one row of the Workspace keys card.
type keyRow struct {
	Name, Workspace, Fingerprint string
	// Short is the fingerprint shortened for the table.
	Short  string
	Active bool
	// Request is the pending request's ID.
	Request              string
	Registered, LastUsed time.Time
	NeverUsed            bool
}

// keysCard is the Workspace keys card of the Workspaces page.
type keysCard struct {
	// Show is false when keys are off and the user has none.
	Show bool
	// Off is true when keys are off but the user has keys.
	Off  bool
	Rows []keyRow
}

func newKeysCard(list gateway.WorkspaceKeyList) keysCard {
	card := keysCard{Show: list.Enabled || len(list.Keys) > 0, Off: !list.Enabled && len(list.Keys) > 0}

	for _, key := range list.Keys {
		row := keyRow{
			Name: key.Name, Workspace: key.Workspace, Fingerprint: key.Fingerprint, Active: key.Status == gateway.KeyStatusActive,
			Short: shortFingerprint(strings.TrimPrefix(key.Fingerprint, "SHA256:")), Request: key.Request, Registered: key.RegisteredAt, NeverUsed: key.LastUsedAt == nil,
		}

		if key.LastUsedAt != nil {
			row.LastUsed = *key.LastUsedAt
		}

		card.Rows = append(card.Rows, row)
	}

	return card
}

// revokeWorkspaceKey revokes the session user's key named in the form. It
// only narrows access, so it needs no step-up. The route's session and
// CSRF checks have run.
func (u *UI) revokeWorkspaceKey(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	name := strings.TrimSpace(request.PostFormValue("name"))
	if name == "" {
		u.renderWorkspaces(writer, request, http.StatusBadRequest, "Name the key.")

		return
	}

	_, err := u.options.Workspaces.RevokeKey(ctx, sessionIdentity(session), name, gateway.ViaUI)

	var refusal *gateway.WorkspaceError

	switch {
	case errors.As(err, &refusal):
		u.renderWorkspaces(writer, request, refusal.Status, refusal.Message)
	case err != nil:
		u.logger.ErrorContext(ctx, "cannot revoke a workspace key", "error", err)
		http.Error(writer, "The gateway can't revoke the key. Try again later.", http.StatusInternalServerError)
	default:
		http.Redirect(writer, request, WorkspacesPath, http.StatusSeeOther)
	}
}
