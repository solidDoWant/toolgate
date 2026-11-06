package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// ReadUnverifiedDefault returns the unverified default of a user_scopes
// row: list when the stored default is none and unverified_list is set,
// else the stored default (docs/design/phase-4.md §2).
func ReadUnverifiedDefault(stored string, list bool) string {
	if stored == ScopeNone && list {
		return ScopeList
	}

	return stored
}

// StoreUnverifiedDefault returns the stored form of an unverified default:
// list is stored as none with unverified_list set, so a replica that knows
// only none and all reads it as none, and a change from it to all still
// widens there. A write of none or all clears unverified_list.
func StoreUnverifiedDefault(value string) (stored string, list bool) {
	if value == ScopeList {
		return ScopeNone, true
	}

	return value, false
}

// DefaultsChangeWidens reports whether a change of the stored defaults
// widens access: a default goes from stored none to all, or the unverified
// default goes from none to list while the allow list has servers. A move
// from list to all widens because list is stored as none.
func DefaultsChangeWidens(oldVerified, newVerified, oldStoredUnverified string, oldList bool, newUnverified string, unverifiedServers int) bool {
	if oldVerified == ScopeNone && newVerified == ScopeAll {
		return true
	}

	if oldStoredUnverified == ScopeNone && newUnverified == ScopeAll {
		return true
	}

	return newUnverified == ScopeList && ReadUnverifiedDefault(oldStoredUnverified, oldList) == ScopeNone && unverifiedServers > 0
}

// validateUnverified checks an allow or a deny of the unverified allow
// list: servers only, at most MaxUnverifiedServers, and for an allow, only
// servers the user has access to.
func (w *Workspaces) validateUnverified(identity auth.Identity, change WorkspaceChange) (WorkspaceChange, error) {
	if change.Action != WorkspaceAllow && change.Action != WorkspaceDeny {
		return change, workspaceError(http.StatusBadRequest, "Only an allow or a deny takes `unverified`.")
	}

	if change.Workspace != "" || change.Session != "" {
		return change, workspaceError(http.StatusBadRequest, "A change of the unverified allow list takes no `workspace` or `session`.")
	}

	if len(change.Servers) == 0 || len(change.Servers) > MaxUnverifiedServers {
		return change, workspaceError(http.StatusBadRequest, "Name 1 to %d servers.", MaxUnverifiedServers)
	}

	for _, server := range change.Servers {
		if server == "" || len(server) > maxServerNameLength {
			return change, workspaceError(http.StatusBadRequest, "A server name has 1 to %d characters.", maxServerNameLength)
		}

		if strings.Contains(server, toolSeparator) {
			return change, workspaceError(http.StatusBadRequest, "The unverified allow list takes whole servers, not tools like `%s`.", server)
		}

		if change.Action == WorkspaceAllow {
			template, err := w.catalog.Offered(server)
			if err != nil || !policy.HasAccess(identity, template.Spec.Access) {
				return change, workspaceError(http.StatusForbidden, "You don't have access to server `%s`.", server)
			}
		}
	}

	change.Servers = slices.Clone(change.Servers)
	slices.Sort(change.Servers)
	change.Servers = slices.Compact(change.Servers)

	return change, nil
}

// changeDefaults runs a validated defaults change in tx, after the
// user_scopes row is locked. A widening change that mode doesn't apply
// becomes a change request, or a scope request when it sets list: a
// replica that knows only none and all can't apply list.
func (w *Workspaces) changeDefaults(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange,
	scope db.UserScope, via string, mode widenMode,
) (WorkspaceResult, error) {
	queries := db.New(tx)

	oldUnverified := ReadUnverifiedDefault(scope.UnverifiedDefault, scope.UnverifiedList)

	verified, unverified := scope.VerifiedDefault, oldUnverified
	if change.Verified != "" {
		verified = change.Verified
	}

	if change.Unverified != "" {
		unverified = change.Unverified
	}

	servers, err := queries.ListUnverifiedServers(ctx, scope.ID)
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("read unverified servers: %w", err)
	}

	if DefaultsChangeWidens(scope.VerifiedDefault, verified, scope.UnverifiedDefault, scope.UnverifiedList, unverified, len(servers)) && mode != widenApply {
		if mode == widenRefuse || change.Unverified != ScopeList {
			return w.notWidened(ctx, tx, identity, change, mode)
		}

		return w.requestUnverified(ctx, tx, identity, ScopeChange{
			Unverified: true, Defaults: &ScopeDefaultsChange{Verified: change.Verified, Unverified: change.Unverified},
		})
	}

	stored, list := StoreUnverifiedDefault(unverified)

	err = queries.SetUserScopeDefaults(ctx, db.SetUserScopeDefaultsParams{
		ID: scope.ID, VerifiedDefault: verified, UnverifiedDefault: stored, UnverifiedList: list,
	})
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("set defaults: %w", err)
	}

	err = audit.NewRecorder(w.auditOutput, queries).Record(ctx, scopeEvent(identity, TypeWorkspaceDefaults, "", map[string]any{
		"old":               map[string]any{"verified": scope.VerifiedDefault, "unverified": oldUnverified},
		"new":               map[string]any{"verified": verified, "unverified": unverified},
		"unverifiedServers": nonNil(servers),
		"via":               via,
	}))
	if err != nil {
		return WorkspaceResult{}, err
	}

	return WorkspaceResult{Applied: true}, notifyUserScope(ctx, tx, scope)
}

// changeUnverified runs a validated allow or deny of the unverified allow
// list in tx, after the user_scopes row is locked. A deny narrows and
// applies at once. An allow of a server not on the list widens: a mode
// that doesn't apply it inserts a scope request.
func (w *Workspaces) changeUnverified(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange,
	scope db.UserScope, via string, mode widenMode,
) (WorkspaceResult, error) {
	queries := db.New(tx)

	current, err := queries.ListUnverifiedServers(ctx, scope.ID)
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("read unverified servers: %w", err)
	}

	var added, removed []string

	if change.Action == WorkspaceDeny {
		for _, server := range change.Servers {
			if slices.Contains(current, server) {
				removed = append(removed, server)
			}
		}

		if err := queries.DeleteUnverifiedServers(ctx, db.DeleteUnverifiedServersParams{ScopeID: scope.ID, Servers: change.Servers}); err != nil {
			return WorkspaceResult{}, fmt.Errorf("delete unverified servers: %w", err)
		}
	} else {
		for _, server := range change.Servers {
			if !slices.Contains(current, server) {
				added = append(added, server)
			}
		}

		if len(added) > 0 && mode != widenApply {
			if mode == widenRefuse {
				return WorkspaceResult{}, ErrWorkspaceChangeWidens
			}

			items := make([]ScopeItem, 0, len(change.Servers))
			for _, server := range change.Servers {
				items = append(items, ScopeItem{Server: server})
			}

			return w.requestUnverified(ctx, tx, identity, ScopeChange{Unverified: true, Items: items})
		}

		if err := addUnverifiedServers(ctx, queries, scope.ID, len(current), added); err != nil {
			return WorkspaceResult{}, err
		}
	}

	if err := queries.TouchUserScope(ctx, scope.ID); err != nil {
		return WorkspaceResult{}, fmt.Errorf("update user scope: %w", err)
	}

	if err := w.recordUnverifiedChange(ctx, queries, identity, added, removed, via); err != nil {
		return WorkspaceResult{}, err
	}

	return WorkspaceResult{Applied: true}, notifyUserScope(ctx, tx, scope)
}

// addUnverifiedServers adds servers to the unverified allow list, which has
// count servers, within MaxUnverifiedServers.
func addUnverifiedServers(ctx context.Context, queries *db.Queries, scopeID int64, count int, servers []string) error {
	if count+len(servers) > MaxUnverifiedServers {
		return workspaceError(http.StatusConflict,
			"Your unverified allow list already has %d servers. Remove one, then try again.", count)
	}

	if _, err := queries.AddUnverifiedServers(ctx, db.AddUnverifiedServersParams{ScopeID: scopeID, Servers: nonNil(servers)}); err != nil {
		return fmt.Errorf("add unverified servers: %w", err)
	}

	return nil
}

// recordUnverifiedChange writes the workspace.change event of a change of
// the unverified allow list.
func (w *Workspaces) recordUnverifiedChange(ctx context.Context, queries *db.Queries, identity auth.Identity, added, removed []string, via string) error {
	return audit.NewRecorder(w.auditOutput, queries).Record(ctx, scopeEvent(identity, TypeWorkspaceChange, "", map[string]any{
		"unverified": true,
		"added":      nonNil(added),
		"removed":    nonNil(removed),
		"cleared":    false,
		"via":        via,
	}))
}

// requestUnverified inserts a pending scope request for a widening change
// of the unverified allow list or the default list, in tx. A replica that
// doesn't know scope requests can't approve it.
func (w *Workspaces) requestUnverified(ctx context.Context, tx pgx.Tx, identity auth.Identity, change ScopeChange) (WorkspaceResult, error) {
	change.AskedBy = identity.Session

	if err := db.New(tx).LockPrincipalAccessRequests(ctx, identity.Principal); err != nil {
		return WorkspaceResult{}, fmt.Errorf("lock access requests: %w", err)
	}

	return w.insertScopeRequest(ctx, tx, identity, change, pgtype.Int8{})
}

// approveUnverified applies an approved scope request of the unverified
// allow list or the default list in tx, after the user_scopes row is
// locked: as the same change applied with a step-up.
func (w *Workspaces) approveUnverified(ctx context.Context, tx pgx.Tx, identity auth.Identity, request db.AccessRequest, change ScopeChange, scope db.UserScope) error {
	if change.Defaults != nil {
		_, err := w.changeDefaults(ctx, tx, identity, WorkspaceChange{
			Action: WorkspaceDefaults, Verified: change.Defaults.Verified, Unverified: change.Defaults.Unverified,
		}, scope, request.ID, widenApply)

		return err
	}

	servers := make([]string, 0, len(change.Items))
	for _, item := range change.Items {
		servers = append(servers, item.Server)
	}

	_, err := w.changeUnverified(ctx, tx, identity, WorkspaceChange{Action: WorkspaceAllow, UnverifiedList: true, Servers: servers}, scope, request.ID, widenApply)

	return err
}

// notifyUserScope notifies scope:<id> of the user_scopes row in tx.
func notifyUserScope(ctx context.Context, tx pgx.Tx, scope db.UserScope) error {
	return store.Notify(ctx, tx, store.KindScope, strconv.FormatInt(scope.ID, 10))
}

// AccessibleServers returns the servers identity has access to, sorted: the
// choices of the unverified allow list on the Workspaces page.
func (w *Workspaces) AccessibleServers(identity auth.Identity) []string {
	var servers []string

	for _, template := range w.catalog.List() {
		if policy.HasAccess(identity, template.Spec.Access) {
			servers = append(servers, template.Name)
		}
	}

	return slices.Compact(servers)
}
