package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// Workspace scoping limits (docs/design/phase-3.md §10, "State").
const (
	// MaxWorkspaceLists is the most workspace lists one principal has.
	MaxWorkspaceLists = 200
	// MaxWorkspaceListServers is the most servers on one list.
	MaxWorkspaceListServers = 64
	// MaxWorkspaceLength is the longest workspace a list may have.
	MaxWorkspaceLength = 256
	// MaxPendingAccessRequestsPerPrincipal is the most pending access
	// requests one principal has, counted apart from approvals.
	MaxPendingAccessRequestsPerPrincipal = 100
	// maxServerNameLength bounds a server name in a change.
	maxServerNameLength = 253
)

// Workspace change actions.
const (
	WorkspaceAllow    = "allow"
	WorkspaceDeny     = "deny"
	WorkspaceClear    = "clear"
	WorkspaceDefaults = "defaults"
)

// Scope defaults (the user_scopes default columns).
const (
	ScopeNone = "none"
	ScopeAll  = "all"
)

// Access-request statuses and kinds (the access_requests columns).
const (
	accessPending    = "pending"
	accessApproved   = "approved"
	accessDenied     = "denied"
	accessExpired    = "expired"
	accessKindChange = "change"
	accessKindServer = "server"
)

// Choices of a server request's approval (the access_requests.decided_scope
// column).
const (
	// AdditionSession allows the server for the request's session only.
	AdditionSession = "session"
	// AdditionWorkspace adds the server to the workspace's list.
	AdditionWorkspace = "workspace"
)

// MaxSessionAdditions is the most servers added to one session for that
// session only: the enabled-server limit.
const MaxSessionAdditions = MaxEnabledServers

// Audit event types of workspace scoping and access requests.
const (
	TypeWorkspaceChange   = "workspace.change"
	TypeWorkspaceDefaults = "workspace.defaults"
	TypeSessionAddition   = "session.addition"
	TypeAccessRequest     = "access.request"
	TypeAccessApprove     = "access.approve"
	TypeAccessDeny        = "access.deny"
)

// Values of a workspace.change event's via.
const (
	ViaUI  = "ui"
	ViaCLI = "cli"
	// ViaTool is a change made by a tool of the built-in server.
	ViaTool = "tool"
)

// WorkspaceChange is a change to a user's workspace lists or defaults
// (docs/design/phase-3.md §10, "API and CLI"). It is also the change JSON of
// a change access request.
type WorkspaceChange struct {
	Action    string `json:"action"`
	Workspace string `json:"workspace,omitempty"`
	// Session is the client session ID whose additions a deny removes, in
	// place of Workspace.
	Session string   `json:"session,omitempty"`
	Servers []string `json:"servers,omitempty"`
	// Verified and Unverified are the new defaults of a defaults change,
	// none or all. Empty leaves a default as it is.
	Verified   string `json:"verified,omitempty"`
	Unverified string `json:"unverified,omitempty"`
}

// WorkspaceError is a change the rules refuse. Status is the HTTP status
// the API answers with, and Message is shown to the user.
type WorkspaceError struct {
	Status  int
	Message string
}

func (e *WorkspaceError) Error() string { return e.Message }

func workspaceError(status int, format string, arguments ...any) *WorkspaceError {
	return &WorkspaceError{Status: status, Message: fmt.Sprintf(format, arguments...)}
}

// WorkspaceResult is the outcome of a change: either it was applied, or a
// pending change request with the ID Request was inserted.
type WorkspaceResult struct {
	Applied bool
	Request string
	// Inserted is true when this call inserted the request, so the user
	// hasn't seen it yet.
	Inserted bool
}

// WorkspaceList is one of a user's workspace lists.
type WorkspaceList struct {
	Workspace string
	Servers   []string
	UpdatedAt time.Time
}

// WorkspaceScopes is a user's defaults and lists.
type WorkspaceScopes struct {
	VerifiedDefault   string
	UnverifiedDefault string
	Lists             []WorkspaceList
	// Entries are the scope entries of each workspace that has any, with or
	// without a list.
	Entries map[string]ScopeEntries
}

// ScopeEntries are the scope entries of one target, by exposed name:
// `<server>__<tool>` for a tool, or the server name for a whole server.
type ScopeEntries struct {
	// Allowed are the allow entries.
	Allowed []string
	// Denied are the deny entries.
	Denied []string
	// UpdatedAt is when the newest entry was made.
	UpdatedAt time.Time
}

// Workspaces reads and writes a user's workspace lists and defaults and
// their change access requests (docs/design/phase-3.md §10).
type Workspaces struct {
	database        *pgxpool.Pool
	catalog         *catalog.Catalog
	auditOutput     io.Writer
	approvalTimeout time.Duration
}

// NewWorkspaces returns the workspaces service. Audit events go to
// auditOutput and the audit_events table. A change request expires after
// approvalTimeout, or DefaultApprovalTimeout when it is zero.
func NewWorkspaces(database *pgxpool.Pool, templates *catalog.Catalog, auditOutput io.Writer, approvalTimeout time.Duration) *Workspaces {
	if auditOutput == nil {
		auditOutput = io.Discard
	}

	if approvalTimeout <= 0 {
		approvalTimeout = DefaultApprovalTimeout
	}

	return &Workspaces{database: database, catalog: templates, auditOutput: auditOutput, approvalTimeout: approvalTimeout}
}

// VerifiedWorkspace returns the identity's verified workspace, or "" when
// it has none.
func VerifiedWorkspace(identity auth.Identity) string {
	if identity.Kind != auth.KindUser || identity.Attestation != auth.AttestationAttested {
		return ""
	}

	return identity.Workspace
}

// Scopes returns the principal's defaults and lists. A principal with no
// user_scopes row has both defaults none and no lists.
func (w *Workspaces) Scopes(ctx context.Context, principal string) (WorkspaceScopes, error) {
	queries := db.New(w.database)
	scopes := WorkspaceScopes{VerifiedDefault: ScopeNone, UnverifiedDefault: ScopeNone, Lists: []WorkspaceList{}, Entries: map[string]ScopeEntries{}}

	row, err := queries.GetUserScope(ctx, principal)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return scopes, nil
	case err != nil:
		return WorkspaceScopes{}, fmt.Errorf("read user scope: %w", err)
	}

	scopes.VerifiedDefault, scopes.UnverifiedDefault = row.VerifiedDefault, row.UnverifiedDefault

	rows, err := queries.ListWorkspaceListsOfPrincipal(ctx, principal)
	if err != nil {
		return WorkspaceScopes{}, fmt.Errorf("read workspace lists: %w", err)
	}

	for _, each := range rows {
		if len(scopes.Lists) == 0 || scopes.Lists[len(scopes.Lists)-1].Workspace != each.Workspace {
			scopes.Lists = append(scopes.Lists, WorkspaceList{Workspace: each.Workspace, Servers: []string{}, UpdatedAt: each.UpdatedAt.Time.UTC()})
		}

		if each.Server.Valid {
			current := &scopes.Lists[len(scopes.Lists)-1]
			current.Servers = append(current.Servers, each.Server.String)
		}
	}

	entries, err := queries.ListScopeEntries(ctx, row.ID)
	if err != nil {
		return WorkspaceScopes{}, fmt.Errorf("read scope entries: %w", err)
	}

	for _, entry := range entries {
		if entry.Workspace.Valid {
			scopes.Entries[entry.Workspace.String] = w.addEntry(scopes.Entries[entry.Workspace.String], entry)
		}
	}

	return scopes, nil
}

// SessionEntries returns the scope entries of each of the sessions, by
// sessions row ID.
func (w *Workspaces) SessionEntries(ctx context.Context, sessionPKs []int64) (map[int64]ScopeEntries, error) {
	rows, err := db.New(w.database).ListScopeEntriesOfSessions(ctx, sessionPKs)
	if err != nil {
		return nil, fmt.Errorf("read scope entries: %w", err)
	}

	entries := map[int64]ScopeEntries{}

	for _, entry := range rows {
		if entry.SessionPk.Valid {
			entries[entry.SessionPk.Int64] = w.addEntry(entries[entry.SessionPk.Int64], entry)
		}
	}

	return entries, nil
}

// addEntry adds entry to entries by its exposed name: a tool's upstream name
// is mapped by the server's current curation, or kept when the catalog
// doesn't offer the server.
func (w *Workspaces) addEntry(entries ScopeEntries, entry db.ScopeEntry) ScopeEntries {
	name := entry.Server

	if entry.Tool != "" {
		tool := entry.Tool
		if template, err := w.catalog.Offered(entry.Server); err == nil {
			tool = toolcatalog.ExposedName(template, entry.Tool)
		}

		name = ProxiedToolName(entry.Server, tool)
	}

	if entries.Allowed == nil {
		entries.Allowed, entries.Denied = []string{}, []string{}
	}

	if entry.Effect == policy.EffectDeny {
		entries.Denied = append(entries.Denied, name)
	} else {
		entries.Allowed = append(entries.Allowed, name)
	}

	if created := entry.CreatedAt.Time.UTC(); created.After(entries.UpdatedAt) {
		entries.UpdatedAt = created
	}

	return entries
}

// Apply applies a change of identity's, widening or narrowing. The caller
// has checked a fresh step-up for a widening change (the web UI). via is
// the workspace.change event's via.
func (w *Workspaces) Apply(ctx context.Context, identity auth.Identity, change WorkspaceChange, via string) error {
	change, err := w.validate(identity, change)
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, w.database, func(tx pgx.Tx) error {
		_, err := w.change(ctx, tx, identity, change, via, widenApply)

		return err
	})
}

// ErrWorkspaceChangeWidens is returned by ApplyNarrowing for a change that
// widens access.
var ErrWorkspaceChangeWidens = errors.New("the workspace change widens access")

// ApplyNarrowing applies a narrowing change of identity's. A widening one
// changes nothing and returns ErrWorkspaceChangeWidens: the web UI calls it
// when the sign-in is too old for a step-up.
func (w *Workspaces) ApplyNarrowing(ctx context.Context, identity auth.Identity, change WorkspaceChange, via string) error {
	change, err := w.validate(identity, change)
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, w.database, func(tx pgx.Tx) error {
		_, err := w.change(ctx, tx, identity, change, via, widenRefuse)

		return err
	})
}

// openWorkspaceRows bounds the open sessions OpenWorkspaces reads.
const openWorkspaceRows = 100

// OpenWorkspaces returns the verified workspaces of principal's open
// sessions, sorted and without duplicates.
func (w *Workspaces) OpenWorkspaces(ctx context.Context, principal string) ([]string, error) {
	rows, err := db.New(w.database).ListOpenSessionsOfPrincipal(ctx, db.ListOpenSessionsOfPrincipalParams{
		Principal: principal, MaxRows: openWorkspaceRows,
	})
	if err != nil {
		return nil, fmt.Errorf("read open sessions: %w", err)
	}

	workspaces := []string{}

	for _, row := range rows {
		if row.Kind == auth.KindUser && row.Attestation == auth.AttestationAttested && row.Workspace != "" {
			workspaces = append(workspaces, row.Workspace)
		}
	}

	slices.Sort(workspaces)

	return slices.Compact(workspaces), nil
}

// Request applies a narrowing change of identity's at once. A widening one
// needs a step-up that a bearer token can't prove, so it inserts a pending
// change access request instead, for the user to approve in the browser.
func (w *Workspaces) Request(ctx context.Context, identity auth.Identity, change WorkspaceChange) (WorkspaceResult, error) {
	if identity.Kind != auth.KindUser {
		return WorkspaceResult{}, workspaceError(http.StatusForbidden, "Only users have workspace lists.")
	}

	if needsScopeRequest(change) {
		target := ScopeTarget{Workspace: change.Workspace, Session: change.Session}
		if target.Workspace == "" && target.Session == "" {
			target.Workspace = VerifiedWorkspace(identity)
			if target.Workspace == "" {
				return WorkspaceResult{}, workspaceError(http.StatusBadRequest, "This request has no verified workspace. Pass `workspace`.")
			}
		}

		return w.RequestScope(ctx, identity, target, change.Servers, "")
	}

	change, err := w.validate(identity, change)
	if err != nil {
		return WorkspaceResult{}, err
	}

	var result WorkspaceResult

	err = pgx.BeginFunc(ctx, w.database, func(tx pgx.Tx) error {
		var err error

		result, err = w.change(ctx, tx, identity, change, ViaCLI, widenRequest)

		return err
	})

	return result, err
}

// needsScopeRequest reports whether a change is an allow that phase 3's
// change request can't hold: one for a session, or naming a tool. It becomes
// a scope request (docs/design/phase-4.md §12).
func needsScopeRequest(change WorkspaceChange) bool {
	if change.Action != WorkspaceAllow {
		return false
	}

	if change.Session != "" {
		return true
	}

	return slices.ContainsFunc(change.Servers, func(item string) bool { return strings.Contains(item, toolSeparator) })
}

// AccessRequest returns the principal's access request with the ID, or
// pgx.ErrNoRows when there is none.
func (w *Workspaces) AccessRequest(ctx context.Context, principal, id string) (db.AccessRequest, error) {
	return db.New(w.database).GetAccessRequestOfPrincipal(ctx, db.GetAccessRequestOfPrincipalParams{ID: id, Principal: principal})
}

// ErrAccessRequestUndecidable is returned for an access request that isn't
// identity's, isn't pending any more, or has expired.
var ErrAccessRequestUndecidable = errors.New("the access request isn't pending")

// ApproveChangeRequest compare-and-sets identity's pending change request
// to approved and applies its change to the current lists and defaults as
// a set operation, in tx. The approval page calls it after checking the
// step-up. Any error leaves tx to be rolled back, so the request stays
// pending; a *WorkspaceError says why to the user.
func (w *Workspaces) ApproveChangeRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error {
	queries := db.New(tx)

	request, err := queries.ApproveAccessRequest(ctx, db.ApproveAccessRequestParams{
		ID: id, Principal: identity.Principal, DecidedBy: pgtype.Text{String: identity.Principal, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("approve access request: %w", err)
	}

	if request.Kind != accessKindChange {
		return ErrAccessRequestUndecidable
	}

	var change WorkspaceChange
	if err := json.Unmarshal(request.Change, &change); err != nil {
		return fmt.Errorf("decode change of access request %s: %w", id, err)
	}

	if _, err := w.change(ctx, tx, identity, change, id, widenApply); err != nil {
		return err
	}

	return w.recordAccessDecision(ctx, tx, identity, request, TypeAccessApprove)
}

// ApproveServerRequest approves identity's pending server request with
// choice, AdditionSession or AdditionWorkspace, and writes its effect in tx
// (docs/design/phase-3.md §10, "Session additions" step 6). It locks the
// request's sessions row, then the user_scopes row, for both choices. The
// approval page calls it after checking the step-up. Any error leaves tx to
// be rolled back, so the request stays pending; a *WorkspaceError says why
// to the user.
func (w *Workspaces) ApproveServerRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id, choice string) error {
	if choice != AdditionSession && choice != AdditionWorkspace {
		return workspaceError(http.StatusBadRequest, "Choose this session or this workspace.")
	}

	queries := db.New(tx)

	pending, err := queries.GetAccessRequestOfPrincipal(ctx, db.GetAccessRequestOfPrincipalParams{ID: id, Principal: identity.Principal})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("read access request: %w", err)
	}

	if pending.Kind != accessKindServer || !pending.SessionPk.Valid || !pending.Server.Valid {
		return ErrAccessRequestUndecidable
	}

	session, err := queries.LockSession(ctx, pending.SessionPk.Int64)
	if err != nil {
		return fmt.Errorf("lock session: %w", err)
	}

	scope, err := lockUserScope(ctx, queries, identity.Principal)
	if err != nil {
		return err
	}

	request, err := queries.ApproveAccessRequest(ctx, db.ApproveAccessRequestParams{
		ID: id, Principal: identity.Principal, DecidedBy: pgtype.Text{String: identity.Principal, Valid: true},
		DecidedScope: pgtype.Text{String: choice, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("approve access request: %w", err)
	}

	server := request.Server.String

	if choice == AdditionSession {
		err = w.addToSession(ctx, queries, tx, identity, request, session, server)
	} else {
		err = w.addToWorkspaceList(ctx, queries, tx, identity, request, scope, server)
	}

	if err != nil {
		return err
	}

	return w.recordAccessDecision(ctx, tx, identity, request, TypeAccessApprove)
}

// addToSession is the "session" choice of a server request: the session
// must still be open in the request's verified workspace.
func (w *Workspaces) addToSession(ctx context.Context, queries *db.Queries, tx pgx.Tx, identity auth.Identity,
	request db.AccessRequest, session db.Session, server string,
) error {
	if session.EndedAt.Valid {
		return workspaceError(http.StatusConflict, "This session has ended.")
	}

	if session.Workspace != request.Workspace || session.Attestation != auth.AttestationAttested || !request.Verified {
		return workspaceError(http.StatusConflict, "This session is now in another workspace.")
	}

	present, err := queries.HasSessionAddition(ctx, db.HasSessionAdditionParams{SessionPk: session.ID, Server: server})
	if err != nil {
		return fmt.Errorf("read session additions: %w", err)
	}

	if !present {
		count, err := queries.CountSessionAdditions(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("count session additions: %w", err)
		}

		if count >= MaxSessionAdditions {
			return workspaceError(http.StatusConflict, "This session already has %d added servers. Remove one, then approve again.", MaxSessionAdditions)
		}
	}

	if err := queries.InsertSessionAddition(ctx, db.InsertSessionAdditionParams{SessionPk: session.ID, Server: server, RequestID: request.ID}); err != nil {
		return fmt.Errorf("insert session addition: %w", err)
	}

	event := scopeEvent(identity, TypeSessionAddition, session.Workspace, map[string]any{"request": request.ID})
	event.Session, event.Attestation, event.Server = session.SessionID, session.Attestation, server

	if err := audit.NewRecorder(w.auditOutput, queries).Record(ctx, event); err != nil {
		return err
	}

	return store.Notify(ctx, tx, store.KindSession, strconv.FormatInt(session.ID, 10))
}

// addToWorkspaceList is the "workspace" choice of a server request: it adds
// the server to the request's workspace list, creating the list when there
// is none, within the list limits.
func (w *Workspaces) addToWorkspaceList(ctx context.Context, queries *db.Queries, tx pgx.Tx, identity auth.Identity,
	request db.AccessRequest, scope db.UserScope, server string,
) error {
	list, err := queries.GetWorkspaceList(ctx, db.GetWorkspaceListParams{ScopeID: scope.ID, Workspace: request.Workspace})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		count, err := queries.CountWorkspaceLists(ctx, scope.ID)
		if err != nil {
			return fmt.Errorf("count workspace lists: %w", err)
		}

		if count >= MaxWorkspaceLists {
			return workspaceError(http.StatusConflict, "You already have %d workspace lists. Remove one, then approve again.", MaxWorkspaceLists)
		}
	case err != nil:
		return fmt.Errorf("read workspace list: %w", err)
	default:
		current, err := queries.ListWorkspaceListServerNames(ctx, list.ID)
		if err != nil {
			return fmt.Errorf("read workspace list: %w", err)
		}

		if !slices.Contains(current, server) && len(current) >= MaxWorkspaceListServers {
			return workspaceError(http.StatusConflict,
				"Workspace `%s`'s list already has %d servers. Remove one, then approve again.", request.Workspace, MaxWorkspaceListServers)
		}
	}

	_, err = w.change(ctx, tx, identity, WorkspaceChange{Action: WorkspaceAllow, Workspace: request.Workspace, Servers: []string{server}}, request.ID, widenApply)

	return err
}

// DenyAccessRequest compare-and-sets identity's pending access request to
// denied in tx, writes access.deny and notifies approval:<id>.
func (w *Workspaces) DenyAccessRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error {
	request, err := db.New(tx).DenyAccessRequest(ctx, db.DenyAccessRequestParams{
		ID: id, Principal: identity.Principal, DecidedBy: pgtype.Text{String: identity.Principal, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("deny access request: %w", err)
	}

	return w.recordAccessDecision(ctx, tx, identity, request, TypeAccessDeny)
}

func (w *Workspaces) recordAccessDecision(ctx context.Context, tx pgx.Tx, identity auth.Identity, request db.AccessRequest, eventType string) error {
	event := accessEvent(identity, eventType, request)
	if err := audit.NewRecorder(w.auditOutput, db.New(tx)).Record(ctx, event); err != nil {
		return err
	}

	return store.Notify(ctx, tx, store.KindApproval, request.ID)
}

// accessEvent is an access-request event, with the fields phase 2 writes
// for an approval event.
func accessEvent(identity auth.Identity, eventType string, request db.AccessRequest) audit.Event {
	return audit.Event{
		Type:        eventType,
		Principal:   request.Principal,
		Workspace:   request.Workspace,
		Session:     identity.Session,
		Attestation: identity.Attestation,
		Server:      request.Server.String,
		Details:     map[string]any{"request": request.ID, "kind": request.Kind},
	}
}

// validate checks a change's fields and returns it with its workspace
// filled in and its servers sorted without duplicates.
func (w *Workspaces) validate(identity auth.Identity, change WorkspaceChange) (WorkspaceChange, error) {
	switch change.Action {
	case WorkspaceDefaults:
		if len(change.Servers) > 0 || change.Workspace != "" || change.Session != "" {
			return change, workspaceError(http.StatusBadRequest, "A defaults change takes only `verified` and `unverified`.")
		}

		for _, value := range []string{change.Verified, change.Unverified} {
			if value != "" && value != ScopeNone && value != ScopeAll {
				return change, workspaceError(http.StatusBadRequest, "A default is `none` or `all`, not `%s`.", value)
			}
		}

		if change.Verified == "" && change.Unverified == "" {
			return change, workspaceError(http.StatusBadRequest, "A defaults change sets `verified`, `unverified` or both.")
		}

		return change, nil
	case WorkspaceAllow, WorkspaceDeny, WorkspaceClear:
	default:
		return change, workspaceError(http.StatusBadRequest, "The action is `allow`, `deny`, `clear` or `defaults`.")
	}

	if change.Verified != "" || change.Unverified != "" {
		return change, workspaceError(http.StatusBadRequest, "Only a defaults change takes `verified` and `unverified`.")
	}

	if change.Action == WorkspaceClear && len(change.Servers) > 0 {
		return change, workspaceError(http.StatusBadRequest, "A clear takes no servers.")
	}

	if change.Action != WorkspaceClear {
		if len(change.Servers) == 0 {
			return change, workspaceError(http.StatusBadRequest, "Name at least one server.")
		}

		if len(change.Servers) > MaxWorkspaceListServers {
			return change, workspaceError(http.StatusBadRequest, "Name at most %d servers.", MaxWorkspaceListServers)
		}

		for _, server := range change.Servers {
			if change.Action == WorkspaceDeny {
				if err := validateScopeItem(server); err != nil {
					return change, err
				}

				continue
			}

			if server == "" || len(server) > maxServerNameLength {
				return change, workspaceError(http.StatusBadRequest, "A server name has 1 to %d characters.", maxServerNameLength)
			}
		}

		change.Servers = slices.Clone(change.Servers)
		slices.Sort(change.Servers)
		change.Servers = slices.Compact(change.Servers)
	}

	if change.Session != "" {
		if change.Action != WorkspaceDeny || change.Workspace != "" {
			return change, workspaceError(http.StatusBadRequest, "Only a deny takes `session`, and not together with `workspace`.")
		}

		return change, nil
	}

	if change.Workspace == "" {
		change.Workspace = VerifiedWorkspace(identity)
		if change.Workspace == "" {
			return change, workspaceError(http.StatusBadRequest, "This request has no verified workspace. Pass `workspace`.")
		}
	}

	if len(change.Workspace) > MaxWorkspaceLength || strings.HasPrefix(change.Workspace, "claimed/") {
		return change, workspaceError(http.StatusBadRequest,
			"Workspace `%s` can't have a list: lists are only for verified workspaces, with names of at most %d characters.",
			truncate(change.Workspace, MaxWorkspaceLength), MaxWorkspaceLength)
	}

	if change.Action == WorkspaceAllow {
		for _, server := range change.Servers {
			template, err := w.catalog.Offered(server)
			if err != nil || !policy.HasAccess(identity, template.Spec.Access) {
				return change, workspaceError(http.StatusForbidden, "You don't have access to server `%s`.", truncate(server, maxServerNameLength))
			}
		}
	}

	return change, nil
}

func truncate(value string, length int) string {
	if len(value) <= length {
		return value
	}

	return value[:length]
}

// change runs a validated change in tx (docs/design/phase-3.md §10,
// "Writes"). mode says what a widening change does.
func (w *Workspaces) change(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange, via string, mode widenMode) (WorkspaceResult, error) {
	queries := db.New(tx)
	applied := WorkspaceResult{Applied: true}

	// A session's additions: the sessions row is locked before the
	// user_scopes row, the order of a session approval.
	var session db.LockOpenSessionOfPrincipalRow

	if change.Session != "" {
		var err error

		session, err = queries.LockOpenSessionOfPrincipal(ctx, db.LockOpenSessionOfPrincipalParams{Principal: identity.Principal, SessionID: change.Session})
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceResult{}, workspaceError(http.StatusNotFound, "You have no open session `%s`.", truncate(change.Session, MaxWorkspaceLength))
		}

		if err != nil {
			return WorkspaceResult{}, fmt.Errorf("lock session: %w", err)
		}
	}

	scope, err := lockUserScope(ctx, queries, identity.Principal)
	if err != nil {
		return WorkspaceResult{}, err
	}

	recorder := audit.NewRecorder(w.auditOutput, queries)
	notifyScope := func() error { return store.Notify(ctx, tx, store.KindScope, strconv.FormatInt(scope.ID, 10)) }

	if change.Action == WorkspaceDefaults {
		verified, unverified := scope.VerifiedDefault, scope.UnverifiedDefault
		if change.Verified != "" {
			verified = change.Verified
		}

		if change.Unverified != "" {
			unverified = change.Unverified
		}

		widens := (scope.VerifiedDefault == ScopeNone && verified == ScopeAll) || (scope.UnverifiedDefault == ScopeNone && unverified == ScopeAll)
		if widens && mode != widenApply {
			return w.notWidened(ctx, tx, identity, change, mode)
		}

		if err := queries.SetUserScopeDefaults(ctx, db.SetUserScopeDefaultsParams{ID: scope.ID, VerifiedDefault: verified, UnverifiedDefault: unverified}); err != nil {
			return WorkspaceResult{}, fmt.Errorf("set defaults: %w", err)
		}

		err := recorder.Record(ctx, scopeEvent(identity, TypeWorkspaceDefaults, "", map[string]any{
			"old": map[string]any{"verified": scope.VerifiedDefault, "unverified": scope.UnverifiedDefault},
			"new": map[string]any{"verified": verified, "unverified": unverified},
			"via": via,
		}))
		if err != nil {
			return WorkspaceResult{}, err
		}

		return applied, notifyScope()
	}

	if change.Action == WorkspaceDeny {
		return applied, w.deny(ctx, tx, identity, change, scope, session, via)
	}

	list, err := queries.GetWorkspaceList(ctx, db.GetWorkspaceListParams{ScopeID: scope.ID, Workspace: change.Workspace})
	hasList := err == nil

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceResult{}, fmt.Errorf("read workspace list: %w", err)
	}

	var current []string

	if hasList {
		current, err = queries.ListWorkspaceListServerNames(ctx, list.ID)
		if err != nil {
			return WorkspaceResult{}, fmt.Errorf("read workspace list: %w", err)
		}
	}

	var added, removed []string

	cleared := false

	switch change.Action {
	case WorkspaceAllow:
		for _, server := range change.Servers {
			if !slices.Contains(current, server) {
				added = append(added, server)
			}
		}

		if len(added) > 0 && mode != widenApply {
			return w.notWidened(ctx, tx, identity, change, mode)
		}

		if !hasList {
			count, err := queries.CountWorkspaceLists(ctx, scope.ID)
			if err != nil {
				return WorkspaceResult{}, fmt.Errorf("count workspace lists: %w", err)
			}

			if count >= MaxWorkspaceLists {
				return WorkspaceResult{}, workspaceError(http.StatusConflict, "You already have %d workspace lists. Remove one, then try again.", MaxWorkspaceLists)
			}

			list, err = queries.InsertWorkspaceList(ctx, db.InsertWorkspaceListParams{ScopeID: scope.ID, Workspace: change.Workspace})
			if err != nil {
				return WorkspaceResult{}, fmt.Errorf("insert workspace list: %w", err)
			}
		}

		if len(current)+len(added) > MaxWorkspaceListServers {
			return WorkspaceResult{}, workspaceError(http.StatusConflict,
				"Workspace `%s`'s list already has %d servers. Remove one, then try again.", change.Workspace, MaxWorkspaceListServers)
		}

		if _, err := queries.AddWorkspaceListServers(ctx, db.AddWorkspaceListServersParams{ListID: list.ID, Servers: added}); err != nil {
			return WorkspaceResult{}, fmt.Errorf("add servers: %w", err)
		}
	case WorkspaceClear:
		if hasList && scope.VerifiedDefault == ScopeAll && mode != widenApply {
			return w.notWidened(ctx, tx, identity, change, mode)
		}

		if hasList {
			if err := queries.DeleteWorkspaceList(ctx, list.ID); err != nil {
				return WorkspaceResult{}, fmt.Errorf("delete workspace list: %w", err)
			}

			removed, cleared = current, true
		}

		// A clear keeps the workspace's deny entries (docs/design/phase-4.md §7).
		err := queries.DeleteWorkspaceScopeEntriesOfEffect(ctx, db.DeleteWorkspaceScopeEntriesOfEffectParams{
			ScopeID: scope.ID, Workspace: pgtype.Text{String: change.Workspace, Valid: true}, Effect: policy.EffectAllow,
		})
		if err != nil {
			return WorkspaceResult{}, fmt.Errorf("delete allow entries: %w", err)
		}
	}

	if hasList && !cleared {
		if err := queries.TouchWorkspaceList(ctx, list.ID); err != nil {
			return WorkspaceResult{}, fmt.Errorf("update workspace list: %w", err)
		}
	}

	if err := queries.TouchUserScope(ctx, scope.ID); err != nil {
		return WorkspaceResult{}, fmt.Errorf("update user scope: %w", err)
	}

	err = recorder.Record(ctx, scopeEvent(identity, TypeWorkspaceChange, change.Workspace, map[string]any{
		"workspace": change.Workspace,
		"added":     nonNil(added),
		"removed":   nonNil(removed),
		"cleared":   cleared,
		"via":       via,
	}))
	if err != nil {
		return WorkspaceResult{}, err
	}

	return applied, notifyScope()
}

// widenMode is what a widening change does.
type widenMode int

const (
	// widenApply applies it: the caller checked a step-up.
	widenApply widenMode = iota
	// widenRequest inserts a pending change request instead.
	widenRequest
	// widenRefuse changes nothing and returns ErrWorkspaceChangeWidens.
	widenRefuse
)

// notWidened handles a widening change that mode doesn't apply.
func (w *Workspaces) notWidened(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange, mode widenMode) (WorkspaceResult, error) {
	if mode == widenRefuse {
		return WorkspaceResult{}, ErrWorkspaceChangeWidens
	}

	return w.requestChange(ctx, tx, identity, change)
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}

// scopeEvent is a scoping event of identity's about workspace.
func scopeEvent(identity auth.Identity, eventType, workspace string, details map[string]any) audit.Event {
	if workspace == "" {
		workspace = identity.Workspace
	}

	return audit.Event{
		Type:        eventType,
		Principal:   identity.Principal,
		Workspace:   workspace,
		Session:     identity.Session,
		Attestation: identity.Attestation,
		Details:     details,
	}
}

// lockUserScope creates the principal's user_scopes row with both defaults
// none unless it exists, and locks it.
func lockUserScope(ctx context.Context, queries *db.Queries, principal string) (db.UserScope, error) {
	err := queries.EnsureUserScope(ctx, db.EnsureUserScopeParams{Principal: principal, VerifiedDefault: ScopeNone, UnverifiedDefault: ScopeNone})
	if err != nil {
		return db.UserScope{}, fmt.Errorf("create user scope: %w", err)
	}

	scope, err := queries.LockUserScope(ctx, principal)
	if err != nil {
		return db.UserScope{}, fmt.Errorf("lock user scope: %w", err)
	}

	return scope, nil
}

// requestChange inserts a pending change request within the principal's
// pending limit, writes access.request and notifies approval:<id>, in tx.
func (w *Workspaces) requestChange(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange) (WorkspaceResult, error) {
	generated, err := uuid.NewV7()
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("access request ID: %w", err)
	}

	id := hex.EncodeToString(generated[:])

	encoded, err := json.Marshal(change)
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("encode change: %w", err)
	}

	queries := db.New(tx)

	if err := queries.LockPrincipalAccessRequests(ctx, identity.Principal); err != nil {
		return WorkspaceResult{}, fmt.Errorf("lock access requests: %w", err)
	}

	workspace := change.Workspace
	if workspace == "" {
		workspace = identity.Workspace
	}

	request, err := queries.InsertChangeRequestWithinLimit(ctx, db.InsertChangeRequestWithinLimitParams{
		ID: id, Principal: identity.Principal, Workspace: workspace,
		Verified:        VerifiedWorkspace(identity) != "",
		Change:          encoded,
		ApprovalTimeout: pgtype.Interval{Microseconds: testtime.Scale(w.approvalTimeout).Microseconds(), Valid: true},
		PendingLimit:    MaxPendingAccessRequestsPerPrincipal,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceResult{}, workspaceError(http.StatusConflict,
			"You already have %d pending access requests. Approve or deny some first.", MaxPendingAccessRequestsPerPrincipal)
	}

	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("insert access request: %w", err)
	}

	if err := audit.NewRecorder(w.auditOutput, queries).Record(ctx, accessEvent(identity, TypeAccessRequest, request)); err != nil {
		return WorkspaceResult{}, err
	}

	if err := store.Notify(ctx, tx, store.KindApproval, id); err != nil {
		return WorkspaceResult{}, err
	}

	return WorkspaceResult{Request: id}, nil
}
