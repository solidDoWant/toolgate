package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
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

// WorkspaceSource reads and writes the user's workspace lists and defaults
// and their change access requests. gateway.Workspaces implements it.
type WorkspaceSource interface {
	// Scopes returns principal's defaults and lists.
	Scopes(ctx context.Context, principal string) (gateway.WorkspaceScopes, error)
	// SessionEntries returns the scope entries of the sessions, by sessions
	// row ID.
	SessionEntries(ctx context.Context, sessionPKs []int64) (map[int64]gateway.ScopeEntries, error)
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
	// ApproveKeyRequest approves a pending key request and makes its key
	// active in tx.
	ApproveKeyRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error
	// PreviewScopeRequest returns a scope request's target and its state
	// before and after the approval.
	PreviewScopeRequest(ctx context.Context, principal string, request db.AccessRequest) (gateway.ScopePreview, error)
	// DenyAccessRequest denies a pending access request in tx.
	DenyAccessRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error
}

func (u *UI) registerWorkspaces() {
	u.Handle("GET "+WorkspacesPath, User, http.HandlerFunc(u.workspacesPage))

	for _, action := range []string{gateway.WorkspaceDefaults, gateway.WorkspaceAllow, gateway.WorkspaceDeny, gateway.WorkspaceClear, workspaceServersAction} {
		u.Handle("POST "+WorkspacesPath+"/"+action, User, u.workspaceChangeHandler(action))
	}
}

// workspaceServersAction is the form that sets a workspace's servers to the
// checked ones. It is not an API action: it becomes allow and deny changes.
const workspaceServersAction = "servers"

// serverChoice is one checkbox of a server the user can use.
type serverChoice struct {
	Name        string
	Description string
	Checked     bool
}

// workspaceCard is one workspace on the Workspaces page.
type workspaceCard struct {
	Workspace workspaceLabel
	// HasList is true when the user chose servers for this workspace;
	// otherwise the verified default applies.
	HasList   bool
	Servers   []string
	UpdatedAt time.Time
	Choices   []serverChoice
}

// pendingChange is a change the user chose before a step-up sign-in, kept
// across it. Nothing is applied until the user confirms it.
type pendingChange struct {
	Action  string
	Summary string
	Fields  [][2]string
	// SignIn is set while the sign-in is too old to confirm.
	SignIn string
}

type workspacesPage struct {
	// VerifiedDefault is "all" or "none".
	VerifiedDefault string
	// UnverifiedDefault is "all", "list" or "none".
	UnverifiedDefault string
	// UnverifiedChoices are the checkboxes of "Only these servers".
	UnverifiedChoices []serverChoice
	Cards             []workspaceCard
	// Descriptions are the catalog descriptions by server name.
	Descriptions map[string]string
	Pending      *pendingChange
	Error        string
}

// pendingFields are the form fields kept across a step-up sign-in.
var pendingFields = []string{"workspace", "server", "verified", "unverified", "unverified_server"}

// maxPendingQuery bounds the kept change so the sign-in's return address
// stays within its limit.
const maxPendingQuery = 1500

// workspacesPage shows the user's workspaces and defaults.
func (u *UI) workspacesPage(writer http.ResponseWriter, request *http.Request) {
	u.renderWorkspaces(writer, request, http.StatusOK, request.URL.Query().Get("error"))
}

// serverDescriptions returns the catalog descriptions by server name.
func (u *UI) serverDescriptions(ctx context.Context, session Session) map[string]string {
	descriptions := map[string]string{}

	if u.options.Servers == nil {
		return descriptions
	}

	infos, err := u.options.Servers.Catalog(ctx, sessionIdentity(session))
	if err != nil {
		u.logger.WarnContext(ctx, "cannot read the catalog for the Workspaces page", "error", err)

		return descriptions
	}

	for _, info := range infos {
		descriptions[info.Name] = info.Description
	}

	return descriptions
}

// serverChoices returns a checkbox for every server in names and checked,
// sorted by name.
func serverChoices(names, checked []string, descriptions map[string]string) []serverChoice {
	all := append(slices.Clone(names), checked...)
	slices.Sort(all)

	var choices []serverChoice

	for _, name := range slices.Compact(all) {
		choices = append(choices, serverChoice{Name: name, Description: descriptions[name], Checked: slices.Contains(checked, name)})
	}

	return choices
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
		Error:             message,
		Pending:           u.pendingChangeOf(request, session),
	}

	accessible := u.options.Workspaces.AccessibleServers(sessionIdentity(session))
	descriptions := u.serverDescriptions(ctx, session)
	data.Descriptions = descriptions
	data.UnverifiedChoices = serverChoices(accessible, scopes.UnverifiedServers, descriptions)

	listed := map[string]bool{}

	for _, list := range scopes.Lists {
		listed[list.Workspace] = true
		data.Cards = append(data.Cards, workspaceCard{
			Workspace: labelWorkspace(list.Workspace), HasList: true, Servers: list.Servers, UpdatedAt: list.UpdatedAt,
			Choices: serverChoices(accessible, list.Servers, descriptions),
		})
	}

	for _, workspace := range open {
		if !listed[workspace] {
			data.Cards = append(data.Cards, workspaceCard{Workspace: labelWorkspace(workspace), Choices: serverChoices(accessible, nil, descriptions)})
		}
	}

	u.renderStatus(writer, request, status, "workspaces", "Workspaces", data)
}

// pendingChangeOf reads a change kept across a step-up from the page's
// query. It reads nothing but the known fields of a known action.
func (u *UI) pendingChangeOf(request *http.Request, session Session) *pendingChange {
	query := request.URL.Query()

	action := query.Get("pending")
	if !slices.Contains([]string{gateway.WorkspaceDefaults, gateway.WorkspaceAllow, gateway.WorkspaceDeny, gateway.WorkspaceClear, workspaceServersAction}, action) {
		return nil
	}

	pending := &pendingChange{Action: action}

	for _, field := range pendingFields {
		for _, value := range query[field] {
			pending.Fields = append(pending.Fields, [2]string{field, value})
		}
	}

	workspace := labelWorkspace(query.Get("workspace")).Label
	servers := strings.Join(query["server"], ", ")

	switch action {
	case workspaceServersAction, gateway.WorkspaceAllow:
		if servers == "" {
			servers = "none"
		}

		pending.Summary = "Servers for " + workspace + ": " + servers + "."
	case gateway.WorkspaceDeny:
		pending.Summary = "Stop using " + servers + " on " + workspace + "."
	case gateway.WorkspaceClear:
		pending.Summary = "Use the default on " + workspace + "."
	default:
		pending.Summary = "New choices for machines without their own choice."
	}

	if StepUpNeeded(session) {
		pending.SignIn = StepUpURL(request.URL.RequestURI())
	}

	return pending
}

// pendingReturnTo is the page the step-up sign-in returns to: the
// Workspaces page with the change the request asked for, so the user need
// not choose it again.
func pendingReturnTo(request *http.Request) string {
	query := url.Values{"pending": {path.Base(request.URL.Path)}}

	for _, field := range pendingFields {
		for _, value := range request.PostForm[field] {
			query.Add(field, value)
		}
	}

	target := WorkspacesPath + "?" + query.Encode()
	if len(target) > maxPendingQuery {
		return WorkspacesPath
	}

	return target
}

// workspaceChangeHandler applies one form's change. With a sign-in at most
// StepUpMaxAge old any change applies; with an older one only a narrowing
// change does, and a widening one is sent to a step-up that returns to the
// page, changing nothing.
func (u *UI) workspaceChangeHandler(action string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		session, _ := SessionFrom(request.Context())
		ctx := request.Context()

		if action == workspaceServersAction {
			u.saveWorkspaceServers(writer, request)

			return
		}

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

	removed, added := diffServers(scopes.UnverifiedServers, request.PostForm["unverified_server"])

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

// diffServers compares the current servers with the checked ones and
// returns the servers to remove and to add, added sorted.
func diffServers(current, posted []string) (removed, added []string) {
	checked := map[string]bool{}
	for _, server := range posted {
		checked[strings.TrimSpace(server)] = true
	}

	for _, server := range current {
		if !checked[server] {
			removed = append(removed, server)
		}
	}

	for server := range checked {
		if server != "" && !slices.Contains(current, server) {
			added = append(added, server)
		}
	}

	slices.Sort(added)

	return removed, added
}

// saveWorkspaceServers sets a workspace's servers to the checked ones. The
// removals run first, so they apply even when the additions need a
// step-up.
func (u *UI) saveWorkspaceServers(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	workspace := strings.TrimSpace(request.PostFormValue("workspace"))
	if workspace == "" {
		u.renderWorkspaces(writer, request, http.StatusBadRequest, "Name the workspace.")

		return
	}

	scopes, err := u.options.Workspaces.Scopes(ctx, session.Principal)
	if err != nil {
		u.answerWorkspaceChange(writer, request, err)

		return
	}

	var current []string

	for _, list := range scopes.Lists {
		if list.Workspace == workspace {
			current = list.Servers
		}
	}

	removed, added := diffServers(current, request.PostForm["server"])

	var changes []gateway.WorkspaceChange

	if len(removed) > 0 {
		changes = append(changes, gateway.WorkspaceChange{Action: gateway.WorkspaceDeny, Workspace: workspace, Servers: removed})
	}

	if len(added) > 0 {
		changes = append(changes, gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: workspace, Servers: added})
	}

	u.answerWorkspaceChange(writer, request, u.applyWorkspaceChanges(ctx, session, changes...))
}

// answerWorkspaceChange answers a form of the Workspaces page after its
// changes ran with err.
func (u *UI) answerWorkspaceChange(writer http.ResponseWriter, request *http.Request, err error) {
	var refusal *gateway.WorkspaceError

	switch {
	case errors.Is(err, gateway.ErrWorkspaceChangeWidens):
		writer.Header().Set("Cache-Control", "no-store")
		http.Redirect(writer, request, StepUpURL(pendingReturnTo(request)), http.StatusSeeOther)
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
	Session string
	// SessionPage is the session's page and SessionLabel is what pages show
	// for it; both are empty for a request without a session.
	SessionPage, SessionLabel string
	CreatedAt, ExpiresAt      time.Time
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
		data.SessionPage = "/ui/sessions/" + strconv.FormatInt(sessionRow.ID, 10)
		data.SessionLabel = labelWorkspace(sessionRow.Workspace).Label
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

	if accessRequest.Kind == "key" {
		u.decideAccessRequest(writer, request, accessRequest, opsmetrics.OutcomeApproved, u.options.Workspaces.ApproveKeyRequest)

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
