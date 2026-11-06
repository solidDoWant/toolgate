package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// accessKindScope is the kind of an access request for a change that a
// phase 3 change request can't describe: a session-scoped or tool-level
// allow (docs/design/phase-4.md §2, "Access requests").
const accessKindScope = "scope"

// MaxAllowScopeEntries is the most allow entries one principal has. Deny
// entries don't count, so a narrowing always works.
const MaxAllowScopeEntries = 500

// ScopeItem is one item of a scope change: a whole server, or one tool of
// it by its upstream name.
type ScopeItem struct {
	Server string `json:"server"`
	Tool   string `json:"tool,omitempty"`
}

// ScopeChange is the change JSON of a scope access request: an allow of
// Items on one target, a workspace or one of the user's sessions.
type ScopeChange struct {
	// Workspace is the target workspace, or the target session's workspace.
	Workspace string `json:"workspace"`
	// Session is the target session's client session ID, or empty for a
	// workspace target.
	Session string      `json:"session,omitempty"`
	Items   []ScopeItem `json:"items"`
	// AskedBy is the client session ID of the session that asked, or empty.
	AskedBy string `json:"askedBy,omitempty"`
	// Unverified makes the request a change of the unverified allow list:
	// an allow of Items' servers, or with Defaults, a defaults change that
	// sets list. Workspace and Session are then empty.
	Unverified bool                 `json:"unverified,omitempty"`
	Defaults   *ScopeDefaultsChange `json:"defaults,omitempty"`
}

// ScopeDefaultsChange is the defaults change of a scope request; an empty
// default stays as it is.
type ScopeDefaultsChange struct {
	Verified   string `json:"verified,omitempty"`
	Unverified string `json:"unverified,omitempty"`
}

// ScopeTarget names the target of a scope allow: exactly one of a
// workspace and one of the caller's open sessions.
type ScopeTarget struct {
	Workspace string
	Session   string
}

var (
	// ErrScopeRequestDenied is returned for a named scope request that was
	// denied.
	ErrScopeRequestDenied = errors.New("the scope request was denied")
	// ErrScopeRequestExpired is returned for a named scope request that has
	// expired.
	ErrScopeRequestExpired = errors.New("the scope request has expired")
)

// RequestScope finds or inserts identity's pending scope request to allow
// items on target (docs/design/phase-4.md §7, "toolgate_allow" step 1).
// Items are server names or `<server>__<tool>` with the tool's exposed name.
// named is the request ID the client named, a lookup key only: it is used
// when every field matches. A named request that was denied or has expired
// returns ErrScopeRequestDenied or ErrScopeRequestExpired with its ID. Else a
// pending request with the same target and items is returned, and only when
// there is none is a new one inserted, with access.request and a
// notification. Nothing else changes until the request is approved.
func (w *Workspaces) RequestScope(ctx context.Context, identity auth.Identity, target ScopeTarget, items []string, named string) (WorkspaceResult, error) {
	if identity.Kind != auth.KindUser {
		return WorkspaceResult{}, workspaceError(http.StatusForbidden, "Only users have workspace lists.")
	}

	if (target.Workspace == "") == (target.Session == "") {
		return WorkspaceResult{}, workspaceError(http.StatusBadRequest, "Name exactly one of `workspace` and `session`.")
	}

	if len(items) == 0 || len(items) > MaxEnabledServers {
		return WorkspaceResult{}, workspaceError(http.StatusBadRequest, "Name 1 to %d servers or tools.", MaxEnabledServers)
	}

	if target.Workspace != "" {
		if err := validateListWorkspace(target.Workspace); err != nil {
			return WorkspaceResult{}, err
		}
	}

	parsed, err := w.allowItems(identity, items)
	if err != nil {
		return WorkspaceResult{}, err
	}

	var result WorkspaceResult

	err = pgx.BeginFunc(ctx, w.database, func(tx pgx.Tx) error {
		var err error

		result, err = w.requestScope(ctx, tx, identity, target, parsed, named)

		return err
	})

	return result, err
}

// validateListWorkspace checks that a workspace can have a list.
func validateListWorkspace(workspace string) error {
	if len(workspace) > MaxWorkspaceLength || strings.HasPrefix(workspace, "claimed/") {
		return workspaceError(http.StatusBadRequest,
			"Workspace `%s` can't have a list: lists are only for verified workspaces, with names of at most %d characters.",
			truncate(workspace, MaxWorkspaceLength), MaxWorkspaceLength)
	}

	return nil
}

// allowItems validates and parses the items of an allow, sorted without
// duplicates. A server the user has no access to, or a tool the server
// doesn't offer, is refused.
func (w *Workspaces) allowItems(identity auth.Identity, items []string) ([]ScopeItem, error) {
	parsed := make([]ScopeItem, 0, len(items))

	for _, item := range items {
		if err := validateScopeItem(item); err != nil {
			return nil, err
		}

		server, tool, isTool := strings.Cut(item, toolSeparator)

		// A server must be offered now. A tool is named by its exposed name
		// in the curation calls use, so while the current generation is
		// invalid it maps with the last valid one (docs/design/phase-4.md §6).
		template, err := w.catalog.Offered(server)
		if isTool {
			if curation, found := w.catalog.Curation(server); found {
				template, err = curation, nil
			}
		}

		if err != nil || !policy.HasAccess(identity, template.Spec.Access) {
			return nil, workspaceError(http.StatusForbidden, "You don't have access to server `%s`.", truncate(server, maxServerNameLength))
		}

		if isTool {
			upstream, offered := toolcatalog.Upstream(template, tool)
			if !offered {
				return nil, workspaceError(http.StatusNotFound, "Server `%s` has no tool `%s`.", server, truncate(tool, maxToolNameLength))
			}

			tool = upstream
		}

		parsed = append(parsed, ScopeItem{Server: server, Tool: tool})
	}

	slices.SortFunc(parsed, func(left, right ScopeItem) int {
		return strings.Compare(left.Server+"\x00"+left.Tool, right.Server+"\x00"+right.Tool)
	})

	return slices.Compact(parsed), nil
}

// requestScope is RequestScope's transaction.
func (w *Workspaces) requestScope(ctx context.Context, tx pgx.Tx, identity auth.Identity, target ScopeTarget, items []ScopeItem, named string) (WorkspaceResult, error) {
	queries := db.New(tx)
	change := ScopeChange{Workspace: target.Workspace, Session: target.Session, Items: items, AskedBy: identity.Session}

	var sessionPK pgtype.Int8

	if target.Session != "" {
		session, err := queries.LockOpenSessionOfPrincipal(ctx, db.LockOpenSessionOfPrincipalParams{Principal: identity.Principal, SessionID: target.Session})
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceResult{}, workspaceError(http.StatusNotFound, "You have no open session `%s`.", truncate(target.Session, MaxWorkspaceLength))
		}

		if err != nil {
			return WorkspaceResult{}, fmt.Errorf("lock session: %w", err)
		}

		if session.Attestation != auth.AttestationAttested || session.Workspace == "" {
			return WorkspaceResult{}, workspaceError(http.StatusConflict,
				"Session `%s` has no verified workspace, so it uses only your default for unverified sessions.", truncate(target.Session, MaxWorkspaceLength))
		}

		change.Workspace = session.Workspace
		sessionPK = pgtype.Int8{Int64: session.ID, Valid: true}
	}

	if err := w.refuseToolsOfDeniedServers(ctx, queries, identity.Principal, change, sessionPK); err != nil {
		return WorkspaceResult{}, err
	}

	encodedItems, err := json.Marshal(items)
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("encode items: %w", err)
	}

	if named != "" {
		request, err := queries.GetAccessRequestOfPrincipal(ctx, db.GetAccessRequestOfPrincipalParams{ID: named, Principal: identity.Principal})
		if err == nil && scopeRequestMatches(request, change, sessionPK) {
			switch request.Status {
			case accessPending:
				return WorkspaceResult{Request: request.ID}, nil
			case accessDenied:
				return WorkspaceResult{Request: request.ID}, ErrScopeRequestDenied
			case accessExpired:
				return WorkspaceResult{Request: request.ID}, ErrScopeRequestExpired
			}
		}

		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return WorkspaceResult{}, fmt.Errorf("read access request: %w", err)
		}
	}

	if err := queries.LockPrincipalAccessRequests(ctx, identity.Principal); err != nil {
		return WorkspaceResult{}, fmt.Errorf("lock access requests: %w", err)
	}

	pending, err := queries.FindPendingScopeRequest(ctx, db.FindPendingScopeRequestParams{
		Principal: identity.Principal, Workspace: change.Workspace, SessionPk: sessionPK, Items: encodedItems,
	})
	if err == nil {
		return WorkspaceResult{Request: pending.ID}, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceResult{}, fmt.Errorf("read access request: %w", err)
	}

	return w.insertScopeRequest(ctx, tx, identity, change, sessionPK)
}

// scopeRequestMatches reports whether a named request is a scope request of
// the same target and items.
func scopeRequestMatches(request db.AccessRequest, change ScopeChange, sessionPK pgtype.Int8) bool {
	if request.Kind != accessKindScope || request.Workspace != change.Workspace || request.SessionPk != sessionPK {
		return false
	}

	var stored ScopeChange
	if err := json.Unmarshal(request.Change, &stored); err != nil {
		return false
	}

	return stored.Session == change.Session && slices.Equal(stored.Items, change.Items)
}

// refuseToolsOfDeniedServers refuses a tool item of a server that has a
// server-level deny entry on the target (docs/design/phase-4.md §7, row 249).
func (w *Workspaces) refuseToolsOfDeniedServers(ctx context.Context, queries *db.Queries, principal string, change ScopeChange, sessionPK pgtype.Int8) error {
	scope, err := queries.GetUserScope(ctx, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read user scope: %w", err)
	}

	entries, err := w.targetEntries(ctx, queries, scope.ID, change, sessionPK)
	if err != nil {
		return err
	}

	for _, item := range change.Items {
		if item.Tool == "" {
			continue
		}

		for _, entry := range entries {
			if entry.Effect == policy.EffectDeny && entry.Server == item.Server && entry.Tool == "" {
				if change.Session != "" {
					return workspaceError(http.StatusConflict, "Server `%s` is denied in this session. Allow the server first.", item.Server)
				}

				return workspaceError(http.StatusConflict, "Server `%s` is denied in workspace `%s`. Allow the server first.", item.Server, change.Workspace)
			}
		}
	}

	return nil
}

// targetEntries returns the scope entries of the change's target.
func (w *Workspaces) targetEntries(ctx context.Context, queries *db.Queries, scopeID int64, change ScopeChange, sessionPK pgtype.Int8) ([]db.ScopeEntry, error) {
	if sessionPK.Valid {
		entries, err := queries.ListScopeEntriesForSession(ctx, sessionPK)
		if err != nil {
			return nil, fmt.Errorf("read scope entries: %w", err)
		}

		return entries, nil
	}

	all, err := queries.ListScopeEntries(ctx, scopeID)
	if err != nil {
		return nil, fmt.Errorf("read scope entries: %w", err)
	}

	return slices.DeleteFunc(all, func(entry db.ScopeEntry) bool {
		return !entry.Workspace.Valid || entry.Workspace.String != change.Workspace
	}), nil
}

// insertScopeRequest inserts a pending scope request within the principal's
// pending limit, writes access.request and notifies approval:<id>, in tx
// after LockPrincipalAccessRequests.
func (w *Workspaces) insertScopeRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, change ScopeChange, sessionPK pgtype.Int8) (WorkspaceResult, error) {
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

	request, err := queries.InsertScopeRequestWithinLimit(ctx, db.InsertScopeRequestWithinLimitParams{
		ID: id, Principal: identity.Principal, SessionPk: sessionPK, Workspace: change.Workspace, Change: encoded,
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

	return WorkspaceResult{Request: id, Inserted: true}, nil
}

// DecodeScopeChange decodes the change JSON of a scope request.
func DecodeScopeChange(encoded []byte) (ScopeChange, error) {
	var change ScopeChange

	err := json.Unmarshal(encoded, &change)

	return change, err
}

// ApproveScopeRequest compare-and-sets identity's pending scope request to
// approved and applies its allow in tx (docs/design/phase-4.md §7, "Nothing
// changes until approval"). It locks a session target, then the user_scopes
// row. The approval page calls it after checking the step-up. Any error
// leaves tx to be rolled back, so the request stays pending; a
// *WorkspaceError says why to the user.
func (w *Workspaces) ApproveScopeRequest(ctx context.Context, tx pgx.Tx, identity auth.Identity, id string) error {
	queries := db.New(tx)

	pending, err := queries.GetAccessRequestOfPrincipal(ctx, db.GetAccessRequestOfPrincipalParams{ID: id, Principal: identity.Principal})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("read access request: %w", err)
	}

	if pending.Kind != accessKindScope {
		return ErrAccessRequestUndecidable
	}

	change, err := DecodeScopeChange(pending.Change)
	if err != nil {
		return fmt.Errorf("decode change of access request %s: %w", id, err)
	}

	var session db.Session

	if pending.SessionPk.Valid {
		session, err = queries.LockSession(ctx, pending.SessionPk.Int64)
		if err != nil {
			return fmt.Errorf("lock session: %w", err)
		}
	}

	scope, err := lockUserScope(ctx, queries, identity.Principal)
	if err != nil {
		return err
	}

	request, err := queries.ApproveAccessRequest(ctx, db.ApproveAccessRequestParams{
		ID: id, Principal: identity.Principal, DecidedBy: pgtype.Text{String: identity.Principal, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestUndecidable
	}

	if err != nil {
		return fmt.Errorf("approve access request: %w", err)
	}

	switch {
	case change.Unverified:
		err = w.approveUnverified(ctx, tx, identity, request, change, scope)
	case pending.SessionPk.Valid:
		err = w.allowInSession(ctx, tx, identity, request, change, scope, session)
	default:
		err = w.allowInWorkspace(ctx, tx, identity, request, change, scope)
	}

	if err != nil {
		return err
	}

	return w.recordAccessDecision(ctx, tx, identity, request, TypeAccessApprove)
}

// insertAllowEntry inserts an allow entry within the principal's limit.
func insertAllowEntry(ctx context.Context, queries *db.Queries, scopeID int64, workspace pgtype.Text, sessionPK pgtype.Int8, item ScopeItem, requestID string) error {
	count, err := queries.CountAllowScopeEntries(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("count allow entries: %w", err)
	}

	if count >= MaxAllowScopeEntries {
		return workspaceError(http.StatusConflict, "You already have %d allowed tools. Remove some, then approve again.", MaxAllowScopeEntries)
	}

	_, err = queries.InsertScopeEntry(ctx, db.InsertScopeEntryParams{
		ScopeID: scopeID, Workspace: workspace, SessionPk: sessionPK, Server: item.Server, Tool: item.Tool,
		Effect: policy.EffectAllow, RequestID: pgtype.Text{String: requestID, Valid: true},
	})
	if err != nil && !isNoRows(err) {
		return fmt.Errorf("insert allow entry: %w", err)
	}

	return nil
}

// deleteDenyEntries deletes the deny entries an approved item lifts: every
// deny of the server for a server item, the tool's deny for a tool item.
func deleteDenyEntries(ctx context.Context, queries *db.Queries, scopeID int64, workspace pgtype.Text, sessionPK pgtype.Int8, item ScopeItem) error {
	_, err := queries.DeleteTargetScopeEntries(ctx, db.DeleteTargetScopeEntriesParams{
		ScopeID: scopeID, Workspace: workspace, SessionPk: sessionPK,
		Server: item.Server, Effect: policy.EffectDeny, AnyTool: item.Tool == "", Tool: item.Tool,
	})
	if err != nil {
		return fmt.Errorf("delete deny entries: %w", err)
	}

	return nil
}

// workspaceReach reports which servers the workspace's list or the verified
// default reaches.
func workspaceReach(ctx context.Context, queries *db.Queries, scope db.UserScope, workspace string) (func(server string) bool, error) {
	list, err := queries.GetWorkspaceList(ctx, db.GetWorkspaceListParams{ScopeID: scope.ID, Workspace: workspace})
	if errors.Is(err, pgx.ErrNoRows) {
		all := scope.VerifiedDefault == ScopeAll

		return func(string) bool { return all }, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read workspace list: %w", err)
	}

	servers, err := queries.ListWorkspaceListServerNames(ctx, list.ID)
	if err != nil {
		return nil, fmt.Errorf("read workspace list: %w", err)
	}

	return func(server string) bool { return slices.Contains(servers, server) }, nil
}

// allowInWorkspace applies an approved allow on a workspace: a server goes
// on the list, created when there is none, and loses its deny entries; a
// tool loses its deny entry and gets an allow entry unless the list or the
// default already reaches its server.
func (w *Workspaces) allowInWorkspace(ctx context.Context, tx pgx.Tx, identity auth.Identity, request db.AccessRequest, change ScopeChange, scope db.UserScope) error {
	queries := db.New(tx)
	workspace := pgtype.Text{String: change.Workspace, Valid: true}

	var servers, tools []string

	for _, item := range change.Items {
		if err := deleteDenyEntries(ctx, queries, scope.ID, workspace, pgtype.Int8{}, item); err != nil {
			return err
		}

		if item.Tool == "" {
			servers = append(servers, item.Server)
		}
	}

	if len(servers) > 0 {
		if err := w.addServersToList(ctx, queries, scope, change.Workspace, servers); err != nil {
			return err
		}
	}

	reaches, err := workspaceReach(ctx, queries, scope, change.Workspace)
	if err != nil {
		return err
	}

	for _, item := range change.Items {
		if item.Tool == "" {
			continue
		}

		tools = append(tools, w.exposedItem(item))

		if reaches(item.Server) {
			continue
		}

		if err := insertAllowEntry(ctx, queries, scope.ID, workspace, pgtype.Int8{}, item, request.ID); err != nil {
			return err
		}
	}

	if err := queries.TouchUserScope(ctx, scope.ID); err != nil {
		return fmt.Errorf("update user scope: %w", err)
	}

	err = audit.NewRecorder(w.auditOutput, queries).Record(ctx, scopeEvent(identity, TypeWorkspaceChange, change.Workspace, map[string]any{
		"workspace": change.Workspace,
		"added":     nonNil(servers),
		"tools":     nonNil(tools),
		"removed":   []string{},
		"cleared":   false,
		"via":       request.ID,
	}))
	if err != nil {
		return err
	}

	return store.Notify(ctx, tx, store.KindScope, strconv.FormatInt(scope.ID, 10))
}

// addServersToList adds servers to the workspace's list, creating the list
// when there is none, within the list limits.
func (w *Workspaces) addServersToList(ctx context.Context, queries *db.Queries, scope db.UserScope, workspace string, servers []string) error {
	list, err := queries.GetWorkspaceList(ctx, db.GetWorkspaceListParams{ScopeID: scope.ID, Workspace: workspace})

	var current []string

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		count, err := queries.CountWorkspaceLists(ctx, scope.ID)
		if err != nil {
			return fmt.Errorf("count workspace lists: %w", err)
		}

		if count >= MaxWorkspaceLists {
			return workspaceError(http.StatusConflict, "You already have %d workspace lists. Remove one, then approve again.", MaxWorkspaceLists)
		}

		list, err = queries.InsertWorkspaceList(ctx, db.InsertWorkspaceListParams{ScopeID: scope.ID, Workspace: workspace})
		if err != nil {
			return fmt.Errorf("insert workspace list: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read workspace list: %w", err)
	default:
		current, err = queries.ListWorkspaceListServerNames(ctx, list.ID)
		if err != nil {
			return fmt.Errorf("read workspace list: %w", err)
		}
	}

	var added []string

	for _, server := range servers {
		if !slices.Contains(current, server) {
			added = append(added, server)
		}
	}

	if len(current)+len(added) > MaxWorkspaceListServers {
		return workspaceError(http.StatusConflict,
			"Workspace `%s`'s list already has %d servers. Remove one, then approve again.", workspace, MaxWorkspaceListServers)
	}

	if _, err := queries.AddWorkspaceListServers(ctx, db.AddWorkspaceListServersParams{ListID: list.ID, Servers: nonNil(added)}); err != nil {
		return fmt.Errorf("add servers: %w", err)
	}

	if err := queries.TouchWorkspaceList(ctx, list.ID); err != nil {
		return fmt.Errorf("update workspace list: %w", err)
	}

	return nil
}

// allowInSession applies an approved allow on a session, after checking the
// session is still open in the request's verified workspace: a server gets a
// session addition and loses the session's deny entries; a tool loses its
// session deny entry and gets a session allow entry unless the session
// already reaches its server.
func (w *Workspaces) allowInSession(ctx context.Context, tx pgx.Tx, identity auth.Identity, request db.AccessRequest,
	change ScopeChange, scope db.UserScope, session db.Session,
) error {
	if session.EndedAt.Valid {
		return workspaceError(http.StatusConflict, "This session has ended.")
	}

	if session.Workspace != request.Workspace || session.Attestation != auth.AttestationAttested || session.SessionID != change.Session {
		return workspaceError(http.StatusConflict, "This session is now in another workspace.")
	}

	queries := db.New(tx)
	sessionPK := pgtype.Int8{Int64: session.ID, Valid: true}

	reaches, err := workspaceReach(ctx, queries, scope, session.Workspace)
	if err != nil {
		return err
	}

	recorder := audit.NewRecorder(w.auditOutput, queries)

	for _, item := range change.Items {
		if err := deleteDenyEntries(ctx, queries, scope.ID, pgtype.Text{}, sessionPK, item); err != nil {
			return err
		}

		details := map[string]any{"request": request.ID, "via": request.ID}

		if item.Tool == "" {
			if err := w.addSessionServer(ctx, queries, session, item.Server, request.ID); err != nil {
				return err
			}
		} else {
			details["tool"] = item.Tool

			added, err := queries.HasSessionAddition(ctx, db.HasSessionAdditionParams{SessionPk: session.ID, Server: item.Server})
			if err != nil {
				return fmt.Errorf("read session additions: %w", err)
			}

			if !added && !reaches(item.Server) {
				if err := insertAllowEntry(ctx, queries, scope.ID, pgtype.Text{}, sessionPK, item, request.ID); err != nil {
					return err
				}
			}
		}

		event := scopeEvent(identity, TypeSessionAddition, session.Workspace, details)
		event.Session, event.Attestation, event.Server = session.SessionID, session.Attestation, item.Server

		if err := recorder.Record(ctx, event); err != nil {
			return err
		}
	}

	return store.Notify(ctx, tx, store.KindSession, strconv.FormatInt(session.ID, 10))
}

// addSessionServer inserts a session addition within the session's limit.
func (w *Workspaces) addSessionServer(ctx context.Context, queries *db.Queries, session db.Session, server, requestID string) error {
	present, err := queries.HasSessionAddition(ctx, db.HasSessionAdditionParams{SessionPk: session.ID, Server: server})
	if err != nil {
		return fmt.Errorf("read session additions: %w", err)
	}

	if present {
		return nil
	}

	count, err := queries.CountSessionAdditions(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("count session additions: %w", err)
	}

	if count >= MaxSessionAdditions {
		return workspaceError(http.StatusConflict, "This session already has %d added servers. Remove one, then approve again.", MaxSessionAdditions)
	}

	if err := queries.InsertSessionAddition(ctx, db.InsertSessionAdditionParams{SessionPk: session.ID, Server: server, RequestID: requestID}); err != nil {
		return fmt.Errorf("insert session addition: %w", err)
	}

	return nil
}

// exposedItem is an item's display name: the server, or
// `<server>__<tool>` with the tool's current exposed name.
func (w *Workspaces) exposedItem(item ScopeItem) string {
	if item.Tool == "" {
		return item.Server
	}

	tool := item.Tool
	if template, found := w.catalog.Curation(item.Server); found {
		tool = toolcatalog.ExposedName(template, item.Tool)
	}

	return ProxiedToolName(item.Server, tool)
}

// ExposedItems are the change's items by display name.
func (w *Workspaces) ExposedItems(change ScopeChange) []string {
	names := make([]string, 0, len(change.Items))
	for _, item := range change.Items {
		names = append(names, w.exposedItem(item))
	}

	return names
}

// ScopeState is what a scope target allows and denies, by display name.
type ScopeState struct {
	// HasList is true when the target workspace has a list. Without one,
	// Default applies to the servers that aren't allowed by name.
	HasList bool
	Default string
	// Allowed are the servers on the list or added to the session, and the
	// allowed tools.
	Allowed []string
	// Denied are the deny entries.
	Denied []string
}

// ScopePreview is a scope request's target, the asking session, and the
// target's state before and after the approval.
type ScopePreview struct {
	Workspace string
	Session   string
	AskedBy   string
	// Unverified is true for a change of the unverified allow list or the
	// default list: then Default is the unverified default.
	Unverified    bool
	Before, After ScopeState
}

// previewUnverified is the preview of a scope request of the unverified
// allow list or the default list.
func previewUnverified(preview ScopePreview, scopes WorkspaceScopes, change ScopeChange) ScopePreview {
	preview.Unverified = true
	preview.Before = ScopeState{Default: scopes.UnverifiedDefault, Allowed: slices.Clone(scopes.UnverifiedServers), Denied: []string{}}
	preview.After = ScopeState{Default: scopes.UnverifiedDefault, Allowed: slices.Clone(scopes.UnverifiedServers), Denied: []string{}}

	if change.Defaults != nil && change.Defaults.Unverified != "" {
		preview.After.Default = change.Defaults.Unverified
	}

	for _, item := range change.Items {
		if !slices.Contains(preview.After.Allowed, item.Server) {
			preview.After.Allowed = append(preview.After.Allowed, item.Server)
		}
	}

	slices.Sort(preview.After.Allowed)

	return preview
}

// PreviewScopeRequest returns the preview of a scope request of principal's
// against the current state.
func (w *Workspaces) PreviewScopeRequest(ctx context.Context, principal string, request db.AccessRequest) (ScopePreview, error) {
	change, err := DecodeScopeChange(request.Change)
	if err != nil {
		return ScopePreview{}, fmt.Errorf("decode change of access request %s: %w", request.ID, err)
	}

	scopes, err := w.Scopes(ctx, principal)
	if err != nil {
		return ScopePreview{}, err
	}

	preview := ScopePreview{Workspace: change.Workspace, Session: change.Session, AskedBy: change.AskedBy}

	if change.Unverified {
		return previewUnverified(preview, scopes, change), nil
	}

	before := ScopeState{Default: scopes.VerifiedDefault, Allowed: []string{}, Denied: []string{}}

	for _, list := range scopes.Lists {
		if list.Workspace == change.Workspace {
			before.HasList = true

			if change.Session == "" {
				before.Allowed = append(before.Allowed, list.Servers...)
			}
		}
	}

	var entries ScopeEntries

	if request.SessionPk.Valid {
		additions, err := db.New(w.database).ListSessionAdditionsOfSessions(ctx, []int64{request.SessionPk.Int64})
		if err != nil {
			return ScopePreview{}, fmt.Errorf("read session additions: %w", err)
		}

		for _, addition := range additions {
			before.Allowed = append(before.Allowed, addition.Server)
		}

		sessionEntries, err := w.SessionEntries(ctx, []int64{request.SessionPk.Int64})
		if err != nil {
			return ScopePreview{}, err
		}

		entries = sessionEntries[request.SessionPk.Int64]
	} else {
		entries = scopes.Entries[change.Workspace]
	}

	before.Allowed = append(before.Allowed, entries.Allowed...)
	before.Denied = append(before.Denied, entries.Denied...)

	after := ScopeState{HasList: before.HasList, Default: before.Default, Allowed: slices.Clone(before.Allowed), Denied: slices.Clone(before.Denied)}

	for _, item := range change.Items {
		name := w.exposedItem(item)
		if !slices.Contains(after.Allowed, name) {
			after.Allowed = append(after.Allowed, name)
		}

		after.Denied = slices.DeleteFunc(after.Denied, func(denied string) bool {
			server, _, _ := strings.Cut(denied, toolSeparator)

			return denied == name || (item.Tool == "" && server == item.Server)
		})

		if item.Tool == "" && change.Session == "" {
			after.HasList = true
		}
	}

	for _, state := range []*ScopeState{&before, &after} {
		slices.Sort(state.Allowed)
		state.Allowed = slices.Compact(state.Allowed)
		slices.Sort(state.Denied)
	}

	preview.Before, preview.After = before, after

	return preview, nil
}
