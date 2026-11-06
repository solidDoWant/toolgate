package gateway

import (
	"context"
	"errors"
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
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// maxToolNameLength bounds a tool name in a scope item.
const maxToolNameLength = 128

// scopeItem is one item of a deny: a whole server, or one tool of it by its
// upstream name.
type scopeItem struct {
	Server string
	Tool   string
}

// validateScopeItem checks an item of the form `<server>` or
// `<server>__<tool>`.
func validateScopeItem(item string) error {
	server, tool, isTool := strings.Cut(item, toolSeparator)
	if server == "" || len(server) > maxServerNameLength {
		return workspaceError(http.StatusBadRequest, "A server name has 1 to %d characters.", maxServerNameLength)
	}

	if isTool && (tool == "" || len(tool) > maxToolNameLength) {
		return workspaceError(http.StatusBadRequest, "A tool name has 1 to %d characters.", maxToolNameLength)
	}

	return nil
}

// scopeItems parses validated items. A tool's exposed name is mapped to its
// upstream name by the server's current curation (docs/design/phase-4.md
// §6). A server the catalog doesn't offer keeps the name as given: a deny
// only narrows, so it needs no access check.
func (w *Workspaces) scopeItems(items []string) []scopeItem {
	parsed := make([]scopeItem, 0, len(items))

	for _, item := range items {
		server, tool, isTool := strings.Cut(item, toolSeparator)
		if isTool {
			if template, err := w.catalog.Offered(server); err == nil {
				tool, _ = toolcatalog.Upstream(template, tool)
			}
		}

		parsed = append(parsed, scopeItem{Server: server, Tool: tool})
	}

	return parsed
}

// deny applies a validated deny in tx, after the target and the user_scopes
// row are locked (docs/design/phase-4.md §7, "toolgate_deny"). It narrows at
// once, with any default and with or without a list, and deny entries don't
// count toward the allow-entry limit.
func (w *Workspaces) deny(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange,
	scope db.UserScope, session db.LockOpenSessionOfPrincipalRow, via string,
) error {
	queries := db.New(tx)
	items := w.scopeItems(change.Servers)

	var servers, denied []string

	for _, item := range items {
		if item.Tool == "" {
			servers = append(servers, item.Server)
			denied = append(denied, item.Server)
		} else {
			denied = append(denied, item.Server+toolSeparator+item.Tool)
		}
	}

	var (
		workspace pgtype.Text
		sessionPK pgtype.Int8
	)

	if change.Session != "" {
		sessionPK = pgtype.Int8{Int64: session.ID, Valid: true}
	} else {
		workspace = pgtype.Text{String: change.Workspace, Valid: true}
	}

	for _, item := range items {
		_, err := queries.InsertScopeEntry(ctx, db.InsertScopeEntryParams{
			ScopeID: scope.ID, Workspace: workspace, SessionPk: sessionPK,
			Server: item.Server, Tool: item.Tool, Effect: policy.EffectDeny,
		})
		if err != nil && !isNoRows(err) {
			return fmt.Errorf("insert deny entry: %w", err)
		}

		// A session tool deny keeps the session's allow entry: the deny
		// entry already refuses the tool, and §7 deletes nothing for it.
		if item.Tool != "" && change.Session != "" {
			continue
		}

		_, err = queries.DeleteTargetScopeEntries(ctx, db.DeleteTargetScopeEntriesParams{
			ScopeID: scope.ID, Workspace: workspace, SessionPk: sessionPK,
			Server: item.Server, Effect: policy.EffectAllow, AnyTool: item.Tool == "", Tool: item.Tool,
		})
		if err != nil {
			return fmt.Errorf("delete allow entries: %w", err)
		}
	}

	if change.Session != "" {
		return w.denyInSession(ctx, tx, identity, change, session, servers, denied)
	}

	return w.denyInWorkspace(ctx, tx, identity, change, scope, servers, denied, via)
}

// denyInSession removes the denied servers' session additions, writes
// session.addition with denied for each item, and notifies session:<id>.
func (w *Workspaces) denyInSession(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange,
	session db.LockOpenSessionOfPrincipalRow, servers, denied []string,
) error {
	queries := db.New(tx)

	removed, err := queries.RemoveSessionAdditions(ctx, db.RemoveSessionAdditionsParams{SessionPk: session.ID, Servers: nonNil(servers)})
	if err != nil {
		return fmt.Errorf("remove session additions: %w", err)
	}

	recorder := audit.NewRecorder(w.auditOutput, queries)

	for _, item := range denied {
		server, tool, _ := strings.Cut(item, toolSeparator)
		details := map[string]any{"denied": true, "removed": tool == "" && slices.Contains(removed, server)}

		if tool != "" {
			details["tool"] = tool
		}

		event := scopeEvent(identity, TypeSessionAddition, session.Workspace, details)
		event.Session, event.Attestation, event.Server = change.Session, session.Attestation, server

		if err := recorder.Record(ctx, event); err != nil {
			return err
		}
	}

	return store.Notify(ctx, tx, store.KindSession, strconv.FormatInt(session.ID, 10))
}

// denyInWorkspace removes the denied servers from the workspace's list when
// it has one, and from the session additions of the user's open sessions in
// the workspace, writes workspace.change with denied, and notifies
// scope:<id>.
func (w *Workspaces) denyInWorkspace(ctx context.Context, tx pgx.Tx, identity auth.Identity, change WorkspaceChange,
	scope db.UserScope, servers, denied []string, via string,
) error {
	queries := db.New(tx)

	list, err := queries.GetWorkspaceList(ctx, db.GetWorkspaceListParams{ScopeID: scope.ID, Workspace: change.Workspace})
	hasList := err == nil

	if err != nil && !isNoRows(err) {
		return fmt.Errorf("read workspace list: %w", err)
	}

	var removed []string

	if hasList {
		removed, err = queries.RemoveWorkspaceListServers(ctx, db.RemoveWorkspaceListServersParams{ListID: list.ID, Servers: nonNil(servers)})
		if err != nil {
			return fmt.Errorf("remove servers: %w", err)
		}

		if err := queries.TouchWorkspaceList(ctx, list.ID); err != nil {
			return fmt.Errorf("update workspace list: %w", err)
		}
	}

	err = queries.RemoveWorkspaceSessionAdditions(ctx, db.RemoveWorkspaceSessionAdditionsParams{
		Principal: identity.Principal, Workspace: change.Workspace, Servers: nonNil(servers),
	})
	if err != nil {
		return fmt.Errorf("remove session additions: %w", err)
	}

	if err := queries.TouchUserScope(ctx, scope.ID); err != nil {
		return fmt.Errorf("update user scope: %w", err)
	}

	err = audit.NewRecorder(w.auditOutput, queries).Record(ctx, scopeEvent(identity, TypeWorkspaceChange, change.Workspace, map[string]any{
		"workspace": change.Workspace,
		"added":     []string{},
		"removed":   nonNil(removed),
		"denied":    denied,
		"cleared":   false,
		"via":       via,
	}))
	if err != nil {
		return err
	}

	return store.Notify(ctx, tx, store.KindScope, strconv.FormatInt(scope.ID, 10))
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
