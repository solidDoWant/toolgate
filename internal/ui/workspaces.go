package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// WorkspacesPath is the Workspaces page.
const WorkspacesPath = "/ui/workspaces"

// unverifiedDefaultNote is the defaults form's warning about sessions that
// leave out the attestation.
const unverifiedDefaultNote = "A workspace's list limits a process holding your token only while the default for unverified sessions is nothing."

// WorkspaceSource reads and writes the user's workspace lists and defaults
// and their change access requests. gateway.Workspaces implements it.
type WorkspaceSource interface {
	// Scopes returns principal's defaults and lists.
	Scopes(ctx context.Context, principal string) (gateway.WorkspaceScopes, error)
	// AccessibleServers returns the servers identity has access to.
	AccessibleServers(identity auth.Identity) []string
	// OpenWorkspaces returns the verified workspaces of principal's open
	// sessions.
	OpenWorkspaces(ctx context.Context, principal string) ([]string, error)
	// Apply applies a change, widening or narrowing. The caller has checked
	// the step-up.
	Apply(ctx context.Context, identity auth.Identity, change gateway.WorkspaceChange, via string) error
	// ApplyNarrowing applies a narrowing change and returns
	// gateway.ErrWorkspaceChangeWidens for a widening one.
	ApplyNarrowing(ctx context.Context, identity auth.Identity, change gateway.WorkspaceChange, via string) error
	// AccessRequest returns principal's access request, or pgx.ErrNoRows.
	AccessRequest(ctx context.Context, principal, id string) (db.AccessRequest, error)
	// ApproveChangeRequest approves a pending change request and applies
	// its change in tx.
	ApproveChangeRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error
	// ApproveServerRequest approves a pending server request for this
	// session or this workspace and writes its effect in tx.
	ApproveServerRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id, choice string) error
	// ApproveScopeRequest approves a pending scope request and applies its
	// allow in tx.
	ApproveScopeRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error
	// PreviewScopeRequest returns a scope request's target and its state
	// before and after the approval.
	PreviewScopeRequest(ctx context.Context, principal string, request db.AccessRequest) (gateway.ScopePreview, error)
	// DenyAccessRequest denies a pending access request in tx.
	DenyAccessRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error
}

func (u *UI) registerWorkspaces() {
	u.Handle("GET "+WorkspacesPath, User, http.HandlerFunc(u.workspacesPage))

	for _, action := range []string{gateway.WorkspaceDefaults, gateway.WorkspaceAllow, gateway.WorkspaceDeny, gateway.WorkspaceClear} {
		u.Handle("POST "+WorkspacesPath+"/"+action, User, u.workspaceChangeHandler(action))
	}
}

// unverifiedChoice is one checkbox of the unverified allow list.
type unverifiedChoice struct {
	Server  string
	Checked bool
}

// workspaceList is one list on the Workspaces page.
type workspaceList struct {
	Workspace string
	Servers   []string
	UpdatedAt time.Time
}

type workspacesPage struct {
	VerifiedDefault   string
	UnverifiedDefault string
	// UnverifiedChoices are the checkboxes of "Only these servers": every
	// server the user has access to, and every server on the list.
	UnverifiedChoices []unverifiedChoice
	Note              string
	Lists             []workspaceList
	// OpenWorkspaces are the verified workspaces of the user's open
	// sessions that have no list yet.
	OpenWorkspaces []string
	// StepUpURL is set when a widening change needs a fresh sign-in first.
	StepUpURL string
	Error     string
}

// workspacesPage shows the user's defaults, lists and the verified
// workspaces of their open sessions.
func (u *UI) workspacesPage(writer http.ResponseWriter, request *http.Request) {
	u.renderWorkspaces(writer, request, http.StatusOK, request.URL.Query().Get("error"))
}

func (u *UI) renderWorkspaces(writer http.ResponseWriter, request *http.Request, status int, message string) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	scopes, err := u.options.Workspaces.Scopes(ctx, session.Principal)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read workspace lists for the web UI", "error", err)
		http.Error(writer, "The gateway can't read your workspace lists. Try again later.", http.StatusInternalServerError)

		return
	}

	open, err := u.options.Workspaces.OpenWorkspaces(ctx, session.Principal)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read open sessions for the web UI", "error", err)
		http.Error(writer, "The gateway can't read your sessions. Try again later.", http.StatusInternalServerError)

		return
	}

	data := workspacesPage{
		VerifiedDefault:   scopes.VerifiedDefault,
		UnverifiedDefault: scopes.UnverifiedDefault,
		Note:              unverifiedDefaultNote,
		Error:             message,
	}

	choices := u.options.Workspaces.AccessibleServers(sessionIdentity(session))
	choices = append(choices, scopes.UnverifiedServers...)
	slices.Sort(choices)

	for _, server := range slices.Compact(choices) {
		data.UnverifiedChoices = append(data.UnverifiedChoices, unverifiedChoice{Server: server, Checked: slices.Contains(scopes.UnverifiedServers, server)})
	}

	listed := map[string]bool{}

	for _, list := range scopes.Lists {
		listed[list.Workspace] = true
		data.Lists = append(data.Lists, workspaceList{Workspace: list.Workspace, Servers: list.Servers, UpdatedAt: list.UpdatedAt})
	}

	for _, workspace := range open {
		if !listed[workspace] {
			data.OpenWorkspaces = append(data.OpenWorkspaces, workspace)
		}
	}

	if StepUpNeeded(session) {
		data.StepUpURL = StepUpURL(WorkspacesPath)
	}

	u.renderStatus(writer, request, status, "workspaces", "Workspaces", data)
}

// workspaceChangeHandler applies one form's change. With a sign-in at most
// StepUpMaxAge old any change applies; with an older one only a narrowing
// change does, and a widening one is sent to a step-up that returns to the
// page, changing nothing.
func (u *UI) workspaceChangeHandler(action string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		session, _ := SessionFrom(request.Context())
		ctx := request.Context()

		change := gateway.WorkspaceChange{Action: action}

		if action == gateway.WorkspaceDefaults {
			change.Verified = request.PostFormValue("verified")
			change.Unverified = request.PostFormValue("unverified")

			if change.Unverified == gateway.ScopeList {
				u.saveUnverifiedDefaults(writer, request, change)

				return
			}
		} else {
			change.Workspace = strings.TrimSpace(request.PostFormValue("workspace"))
			if change.Workspace == "" {
				u.renderWorkspaces(writer, request, http.StatusBadRequest, "Name the workspace.")

				return
			}

			for _, server := range request.PostForm["server"] {
				if server = strings.TrimSpace(server); server != "" {
					change.Servers = append(change.Servers, server)
				}
			}
		}

		u.answerWorkspaceChange(writer, request, u.applyWorkspaceChanges(ctx, session, change))
	})
}

// applyWorkspaceChanges applies changes in order with the session's
// identity: any change with a sign-in at most StepUpMaxAge old, else only
// narrowing ones. It stops at the first error.
func (u *UI) applyWorkspaceChanges(ctx context.Context, session Session, changes ...gateway.WorkspaceChange) error {
	apply := u.options.Workspaces.ApplyNarrowing
	if session.freshFor(StepUpMaxAge, time.Now()) {
		apply = u.options.Workspaces.Apply
	}

	for _, change := range changes {
		if err := apply(ctx, sessionIdentity(session), change, gateway.ViaUI); err != nil {
			return err
		}
	}

	return nil
}

// saveUnverifiedDefaults saves the defaults form with "Only these
// servers": the unverified allow list becomes the checked servers, then the
// defaults are set. The narrowing part, removing servers, runs first, so
// it applies even when the rest needs a step-up.
func (u *UI) saveUnverifiedDefaults(writer http.ResponseWriter, request *http.Request, defaults gateway.WorkspaceChange) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	scopes, err := u.options.Workspaces.Scopes(ctx, session.Principal)
	if err != nil {
		u.answerWorkspaceChange(writer, request, err)

		return
	}

	checked := map[string]bool{}
	for _, server := range request.PostForm["unverified_server"] {
		checked[strings.TrimSpace(server)] = true
	}

	var removed, added []string

	for _, server := range scopes.UnverifiedServers {
		if !checked[server] {
			removed = append(removed, server)
		}
	}

	for server := range checked {
		if server != "" && !slices.Contains(scopes.UnverifiedServers, server) {
			added = append(added, server)
		}
	}

	slices.Sort(added)

	var changes []gateway.WorkspaceChange

	if len(removed) > 0 {
		changes = append(changes, gateway.WorkspaceChange{Action: gateway.WorkspaceDeny, UnverifiedList: true, Servers: removed})
	}

	if len(added) > 0 {
		changes = append(changes, gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: added})
	}

	changes = append(changes, defaults)

	u.answerWorkspaceChange(writer, request, u.applyWorkspaceChanges(ctx, session, changes...))
}

// answerWorkspaceChange answers a form of the Workspaces page after its
// changes ran with err.
func (u *UI) answerWorkspaceChange(writer http.ResponseWriter, request *http.Request, err error) {
	var refusal *gateway.WorkspaceError

	switch {
	case errors.Is(err, gateway.ErrWorkspaceChangeWidens):
		writer.Header().Set("Cache-Control", "no-store")
		http.Redirect(writer, request, StepUpURL(WorkspacesPath), http.StatusSeeOther)
	case errors.As(err, &refusal):
		u.renderWorkspaces(writer, request, refusal.Status, refusal.Message)
	case err != nil:
		u.logger.ErrorContext(request.Context(), "cannot change a workspace list", "error", err)
		http.Error(writer, "The gateway can't save the change. Try again later.", http.StatusInternalServerError)
	default:
		http.Redirect(writer, request, WorkspacesPath, http.StatusSeeOther)
	}
}

// Texts the page of an access request shows.
const (
	accessRequestExpiredMessage  = "This request expired."
	accessRequestApprovedMessage = "This request was approved."
	accessRequestDeniedMessage   = "This request was denied."
)

// listState is a workspace list, or its absence, on the approval page.
type listState struct {
	HasList bool
	Servers []string
	// Default is the verified default, which applies without a list.
	Default string
}

// changePreview is a change request's effect, before and after.
type changePreview struct {
	Defaults                          bool
	Workspace                         string
	BeforeVerified, AfterVerified     string
	BeforeUnverified, AfterUnverified string
	Before, After                     listState
}

type accessRequestPage struct {
	ID, Kind, Workspace, Server string
	// Session is the client session ID of a server request.
	Session              string
	CreatedAt, ExpiresAt time.Time
	// Message is the request's status when it can't be decided any more.
	Message string
	// Error says why the last approval didn't apply.
	Error     string
	StepUpURL string
	Change    *changePreview
	// Scope is the preview of a scope request.
	Scope *gateway.ScopePreview
}

// servedAccessRequest serves the request at /ui/approvals/{id} with serve
// when the ID names no approval of the user's but one of their access
// requests, and gives 404 when it names neither. It returns false when the
// ID names an approval, which the caller then serves.
func (u *UI) servedAccessRequest(writer http.ResponseWriter, request *http.Request,
	serve func(http.ResponseWriter, *http.Request, db.AccessRequest),
) bool {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()
	id := request.PathValue("id")

	_, err := u.queries.GetApprovalOfPrincipal(ctx, db.GetApprovalOfPrincipalParams{ID: id, Principal: session.Principal})
	if err == nil || !errors.Is(err, pgx.ErrNoRows) || u.options.Workspaces == nil {
		// An approval, a read error or no access requests: the approval
		// handler answers.
		return false
	}

	accessRequest, err := u.options.Workspaces.AccessRequest(ctx, session.Principal, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(writer, request)

		return true
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read an access request for the web UI", "error", err)
		http.Error(writer, "The gateway can't read this request. Try again later.", http.StatusInternalServerError)

		return true
	}

	serve(writer, request, accessRequest)

	return true
}

func (u *UI) accessRequestDetail(writer http.ResponseWriter, request *http.Request, accessRequest db.AccessRequest) {
	u.renderAccessRequest(writer, request, http.StatusOK, accessRequest, "")
}

func (u *UI) renderAccessRequest(writer http.ResponseWriter, request *http.Request, status int, accessRequest db.AccessRequest, message string) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	data := accessRequestPage{
		ID:        accessRequest.ID,
		Kind:      accessRequest.Kind,
		Workspace: accessRequest.Workspace,
		Server:    accessRequest.Server.String,
		CreatedAt: accessRequest.CreatedAt.Time,
		ExpiresAt: accessRequest.ExpiresAt.Time,
		Message:   accessRequestStatusMessage(accessRequest, time.Now()),
		Error:     message,
	}

	if accessRequest.SessionPk.Valid {
		sessionRow, err := u.queries.GetSessionByID(ctx, accessRequest.SessionPk.Int64)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot read the session of an access request", "request", accessRequest.ID, "error", err)
			http.Error(writer, "The gateway can't read this request. Try again later.", http.StatusInternalServerError)

			return
		}

		data.Session = sessionRow.SessionID
	}

	if accessRequest.Kind == "change" {
		change, err := decodeChange(accessRequest.Change)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot decode a change request", "request", accessRequest.ID, "error", err)
			http.Error(writer, "The gateway can't read this request. Try again later.", http.StatusInternalServerError)

			return
		}

		scopes, err := u.options.Workspaces.Scopes(ctx, session.Principal)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot read workspace lists for the web UI", "error", err)
			http.Error(writer, "The gateway can't read your workspace lists. Try again later.", http.StatusInternalServerError)

			return
		}

		preview := previewChange(scopes, change)
		data.Change = &preview
	}

	if accessRequest.Kind == "scope" {
		preview, err := u.options.Workspaces.PreviewScopeRequest(ctx, session.Principal, accessRequest)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot preview a scope request", "request", accessRequest.ID, "error", err)
			http.Error(writer, "The gateway can't read this request. Try again later.", http.StatusInternalServerError)

			return
		}

		// The session row of a scope request is its target; the session
		// that asked is shown on its own.
		data.Session = ""
		data.Scope = &preview
	}

	if data.Message == "" && StepUpNeeded(session) {
		data.StepUpURL = StepUpURL(ApprovalPath(accessRequest.ID))
	}

	u.renderStatus(writer, request, status, "access_request", "Access request", data)
}

func decodeChange(encoded []byte) (gateway.WorkspaceChange, error) {
	var change gateway.WorkspaceChange

	err := json.Unmarshal(encoded, &change)

	return change, err
}

// accessRequestStatusMessage is the request's real status, or empty while
// it is pending and can still be decided.
func accessRequestStatusMessage(accessRequest db.AccessRequest, now time.Time) string {
	switch accessRequest.Status {
	case "pending":
		if !accessRequest.ExpiresAt.Time.After(now) {
			return accessRequestExpiredMessage
		}

		return ""
	case "approved":
		return accessRequestApprovedMessage
	case "denied":
		return accessRequestDeniedMessage
	default:
		return accessRequestExpiredMessage
	}
}

// previewChange applies change to the current scopes as the approval
// would, as a set operation, and returns both states.
func previewChange(scopes gateway.WorkspaceScopes, change gateway.WorkspaceChange) changePreview {
	preview := changePreview{Workspace: change.Workspace}

	if change.Action == gateway.WorkspaceDefaults {
		preview.Defaults = true
		preview.BeforeVerified, preview.BeforeUnverified = scopes.VerifiedDefault, scopes.UnverifiedDefault
		preview.AfterVerified, preview.AfterUnverified = scopes.VerifiedDefault, scopes.UnverifiedDefault

		if change.Verified != "" {
			preview.AfterVerified = change.Verified
		}

		if change.Unverified != "" {
			preview.AfterUnverified = change.Unverified
		}

		return preview
	}

	for _, list := range scopes.Lists {
		if list.Workspace == change.Workspace {
			preview.Before = listState{HasList: true, Servers: slices.Clone(list.Servers)}
		}
	}

	preview.Before.Default = scopes.VerifiedDefault
	after := listState{HasList: preview.Before.HasList, Servers: slices.Clone(preview.Before.Servers), Default: scopes.VerifiedDefault}

	switch change.Action {
	case gateway.WorkspaceAllow:
		after.HasList = true

		for _, server := range change.Servers {
			if !slices.Contains(after.Servers, server) {
				after.Servers = append(after.Servers, server)
			}
		}

		slices.Sort(after.Servers)
	case gateway.WorkspaceDeny:
		after.Servers = slices.DeleteFunc(after.Servers, func(server string) bool { return slices.Contains(change.Servers, server) })
	case gateway.WorkspaceClear:
		after = listState{Default: scopes.VerifiedDefault}
	}

	preview.After = after

	return preview
}

// approveAccessRequest approves a pending change request and applies its
// change, or a server request with the chosen scope and its effect, in the
// compare-and-set's transaction. The route's RequireStepUp
// has checked the fresh sign-in.
func (u *UI) approveAccessRequest(writer http.ResponseWriter, request *http.Request, accessRequest db.AccessRequest) {
	if accessRequest.Kind == "server" {
		// "Allow for this session" or "Always allow in this workspace".
		choice := request.PostFormValue("scope")

		u.decideAccessRequest(writer, request, accessRequest, opsmetrics.OutcomeApproved, func(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error {
			return u.options.Workspaces.ApproveServerRequest(ctx, tx, identity, id, choice)
		})

		return
	}

	if accessRequest.Kind == "scope" {
		u.decideAccessRequest(writer, request, accessRequest, opsmetrics.OutcomeApproved, u.options.Workspaces.ApproveScopeRequest)

		return
	}

	u.decideAccessRequest(writer, request, accessRequest, opsmetrics.OutcomeApproved, u.options.Workspaces.ApproveChangeRequest)
}

// denyAccessRequest denies a pending access request. It needs no step-up.
func (u *UI) denyAccessRequest(writer http.ResponseWriter, request *http.Request, accessRequest db.AccessRequest) {
	u.decideAccessRequest(writer, request, accessRequest, opsmetrics.OutcomeDenied, u.options.Workspaces.DenyAccessRequest)
}

func (u *UI) decideAccessRequest(writer http.ResponseWriter, request *http.Request, accessRequest db.AccessRequest, outcome string,
	decide func(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error,
) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	err := u.inTransaction(ctx, func(tx pgx.Tx) error {
		return decide(ctx, tx, sessionIdentity(session), accessRequest.ID)
	})

	var refusal *gateway.WorkspaceError

	switch {
	case errors.Is(err, gateway.ErrAccessRequestUndecidable):
		u.rerenderAccessRequest(writer, request, accessRequest.ID, "")
	case errors.As(err, &refusal):
		u.rerenderAccessRequest(writer, request, accessRequest.ID, refusal.Message)
	case err != nil:
		u.logger.ErrorContext(ctx, "cannot decide an access request", "error", err)
		http.Error(writer, "The gateway can't save your decision. Try again later.", http.StatusInternalServerError)
	default:
		opsmetrics.ApprovalOutcome(accessRequest.Kind, outcome, 1)
		http.Redirect(writer, request, ApprovalPath(accessRequest.ID), http.StatusSeeOther)
	}
}

// rerenderAccessRequest shows the request as it is now, with status 409.
func (u *UI) rerenderAccessRequest(writer http.ResponseWriter, request *http.Request, id, message string) {
	session, _ := SessionFrom(request.Context())

	accessRequest, err := u.options.Workspaces.AccessRequest(request.Context(), session.Principal, id)
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot read an access request for the web UI", "error", err)
		http.Error(writer, "The gateway can't read this request. Try again later.", http.StatusInternalServerError)

		return
	}

	u.renderAccessRequest(writer, request, http.StatusConflict, accessRequest, message)
}
