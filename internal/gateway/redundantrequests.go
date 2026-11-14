package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// settleRedundantRequests closes, as approved, each other pending access
// request of identity's whose change the approval of decided has put in
// effect, so the user isn't asked again for what already holds. The closed
// request's access.approve event names decided as satisfiedBy. It runs in
// the approving transaction, after the change is applied, with the
// user_scopes row locked.
func (w *Workspaces) settleRedundantRequests(ctx context.Context, tx pgx.Tx, identity auth.Identity, decided string) error {
	queries := db.New(tx)

	rows, err := queries.ListPendingAccessRequestsOfPrincipal(ctx, identity.Principal)
	if err != nil {
		return fmt.Errorf("read pending access requests: %w", err)
	}

	if len(rows) == 0 || (len(rows) == 1 && rows[0].AccessRequest.ID == decided) {
		return nil
	}

	state, err := readScopeState(ctx, queries, identity.Principal)
	if err != nil {
		return err
	}

	for _, row := range rows {
		pending := row.AccessRequest
		if pending.ID == decided || !state.inEffect(pending) {
			continue
		}

		request, err := queries.ApproveAccessRequest(ctx, db.ApproveAccessRequestParams{
			ID: pending.ID, Principal: identity.Principal, DecidedBy: pgtype.Text{String: identity.Principal, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}

		if err != nil {
			return fmt.Errorf("approve access request: %w", err)
		}

		event := accessEvent(identity, TypeAccessApprove, request)
		event.Details["satisfiedBy"] = decided

		if err := audit.NewRecorder(w.auditOutput, queries).Record(ctx, event); err != nil {
			return err
		}

		if err := store.Notify(ctx, tx, store.KindApproval, request.ID); err != nil {
			return err
		}
	}

	return nil
}

// scopeState is what a principal's scoping allows now: the defaults, the
// unverified allow list, and each workspace's list and denied servers.
type scopeState struct {
	verifiedDefault   string
	unverifiedDefault string
	unverifiedServers []string
	lists             map[string][]string
	deniedServers     map[string][]string
}

func readScopeState(ctx context.Context, queries *db.Queries, principal string) (scopeState, error) {
	scope, err := queries.GetUserScope(ctx, principal)
	if err != nil {
		return scopeState{}, fmt.Errorf("read user scope: %w", err)
	}

	state := scopeState{
		verifiedDefault:   scope.VerifiedDefault,
		unverifiedDefault: ReadUnverifiedDefault(scope.UnverifiedDefault, scope.UnverifiedList),
		lists:             map[string][]string{},
		deniedServers:     map[string][]string{},
	}

	state.unverifiedServers, err = queries.ListUnverifiedServers(ctx, scope.ID)
	if err != nil {
		return scopeState{}, fmt.Errorf("read unverified servers: %w", err)
	}

	lists, err := queries.ListWorkspaceListsOfPrincipal(ctx, principal)
	if err != nil {
		return scopeState{}, fmt.Errorf("read workspace lists: %w", err)
	}

	for _, list := range lists {
		if list.Server.Valid {
			state.lists[list.Workspace] = append(state.lists[list.Workspace], list.Server.String)
		}
	}

	entries, err := queries.ListScopeEntries(ctx, scope.ID)
	if err != nil {
		return scopeState{}, fmt.Errorf("read scope entries: %w", err)
	}

	for _, entry := range entries {
		if entry.Effect == policy.EffectDeny && entry.Workspace.Valid {
			state.deniedServers[entry.Workspace.String] = append(state.deniedServers[entry.Workspace.String], entry.Server)
		}
	}

	return state, nil
}

// inEffect reports whether a pending request's change already holds, so
// approving it would change nothing. Requests whose effect it can't judge
// from the lists and defaults alone, such as tool or session allows and
// clears, are never in effect. A server request is never closed this way:
// it stays pending and expires unused, and the enable waiting on it wakes
// on the scope change and passes.
func (s scopeState) inEffect(request db.AccessRequest) bool {
	switch request.Kind {
	case accessKindChange:
		var change WorkspaceChange
		if err := json.Unmarshal(request.Change, &change); err != nil {
			return false
		}

		switch {
		case change.Action == WorkspaceDefaults:
			return s.defaultsHold(change.Verified, change.Unverified)
		case change.Action != WorkspaceAllow || len(change.Servers) == 0 || change.Session != "":
			return false
		case change.UnverifiedList:
			return containsAll(s.unverifiedServers, change.Servers)
		default:
			return change.Workspace != "" && !slices.ContainsFunc(change.Servers, func(server string) bool { return !s.listed(change.Workspace, server) })
		}
	case accessKindScope:
		change, err := DecodeScopeChange(request.Change)
		if err != nil {
			return false
		}

		return s.scopeChangeHolds(change, request.SessionPk.Valid)
	}

	return false
}

func (s scopeState) scopeChangeHolds(change ScopeChange, sessionTarget bool) bool {
	if change.Defaults != nil {
		return change.Unverified && s.defaultsHold(change.Defaults.Verified, change.Defaults.Unverified)
	}

	if len(change.Items) == 0 || sessionTarget || change.Session != "" {
		return false
	}

	servers := make([]string, 0, len(change.Items))

	for _, item := range change.Items {
		if item.Tool != "" {
			return false
		}

		servers = append(servers, item.Server)
	}

	if change.Unverified {
		return containsAll(s.unverifiedServers, servers)
	}

	for _, server := range servers {
		// The approval of a server item also deletes its deny entries.
		if !s.listed(change.Workspace, server) || slices.Contains(s.deniedServers[change.Workspace], server) {
			return false
		}
	}

	return change.Workspace != ""
}

// listed reports whether server is on workspace's list.
func (s scopeState) listed(workspace, server string) bool {
	return slices.Contains(s.lists[workspace], server)
}

// defaultsHold reports whether the defaults are already the ones a
// defaults change sets; an empty value leaves a default as it is.
func (s scopeState) defaultsHold(verified, unverified string) bool {
	return (verified == "" || verified == s.verifiedDefault) && (unverified == "" || unverified == s.unverifiedDefault)
}

func containsAll(have, want []string) bool {
	return !slices.ContainsFunc(want, func(value string) bool { return !slices.Contains(have, value) })
}
