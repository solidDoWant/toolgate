package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// MaxSummaryWorkspaces and MaxSummarySessions bound the workspaces and the
// sessions of a scope summary.
const (
	MaxSummaryWorkspaces = 200
	MaxSummarySessions   = 200
)

// ScopeList is the unverified default `list`: the stored default is none
// with the allow list turned on.
const ScopeList = "list"

// ScopeSummary is everything a user's own scope holds: the defaults, each
// workspace's list and entries, and each open session's additions and
// entries. Tool entries are shown as `<server>__<exposed name>`.
type ScopeSummary struct {
	Defaults   ScopeDefaults      `json:"defaults"`
	Workspaces []WorkspaceSummary `json:"workspaces"`
	Sessions   []SessionSummary   `json:"sessions"`
	Truncated  bool               `json:"truncated"`
}

// ScopeDefaults are a user's defaults and unverified allow list.
type ScopeDefaults struct {
	Verified          string   `json:"verified"`
	Unverified        string   `json:"unverified"`
	UnverifiedServers []string `json:"unverifiedServers"`
}

// WorkspaceSummary is one workspace of a scope summary.
type WorkspaceSummary struct {
	Workspace string   `json:"workspace"`
	Servers   []string `json:"servers"`
	Tools     []string `json:"tools"`
	Denied    []string `json:"denied"`
}

// SessionSummary is one open session of a scope summary.
type SessionSummary struct {
	Session    string    `json:"session"`
	Workspace  string    `json:"workspace"`
	Verified   bool      `json:"verified"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Allowed    []string  `json:"allowed"`
	Denied     []string  `json:"denied"`
}

// ScopeSummary returns principal's own scope: its defaults and lists from
// Scopes, its scope entries, and its open sessions as GET /api/v1/sessions
// reads them (seen within idleTimeout). It reads nothing of another
// principal.
func (w *Workspaces) ScopeSummary(ctx context.Context, principal string, idleTimeout time.Duration) (ScopeSummary, error) {
	scopes, err := w.Scopes(ctx, principal)
	if err != nil {
		return ScopeSummary{}, err
	}

	summary := ScopeSummary{
		Defaults:   ScopeDefaults{Verified: scopes.VerifiedDefault, Unverified: scopes.UnverifiedDefault, UnverifiedServers: []string{}},
		Workspaces: []WorkspaceSummary{},
		Sessions:   []SessionSummary{},
	}

	queries := db.New(w.database)

	var entries []db.ScopeEntry

	row, err := queries.GetUserScope(ctx, principal)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return ScopeSummary{}, fmt.Errorf("read user scope: %w", err)
	default:
		if row.UnverifiedList && row.UnverifiedDefault == ScopeNone {
			summary.Defaults.Unverified = ScopeList
		}

		servers, err := queries.ListUnverifiedServers(ctx, row.ID)
		if err != nil {
			return ScopeSummary{}, fmt.Errorf("read unverified servers: %w", err)
		}

		summary.Defaults.UnverifiedServers = append(summary.Defaults.UnverifiedServers, servers...)

		entries, err = queries.ListScopeEntries(ctx, row.ID)
		if err != nil {
			return ScopeSummary{}, fmt.Errorf("read scope entries: %w", err)
		}
	}

	summary.Workspaces, summary.Truncated = w.workspaceSummaries(scopes.Lists, entries)

	sessions, truncated, err := w.sessionSummaries(ctx, queries, principal, idleTimeout, entries)
	if err != nil {
		return ScopeSummary{}, err
	}

	summary.Sessions = sessions
	summary.Truncated = summary.Truncated || truncated

	return summary, nil
}

// workspaceSummaries joins the workspace lists and the workspace entries,
// sorted by workspace, at most MaxSummaryWorkspaces of them.
func (w *Workspaces) workspaceSummaries(lists []WorkspaceList, entries []db.ScopeEntry) ([]WorkspaceSummary, bool) {
	byName := map[string]*WorkspaceSummary{}

	get := func(workspace string) *WorkspaceSummary {
		summary, found := byName[workspace]
		if !found {
			summary = &WorkspaceSummary{Workspace: workspace, Servers: []string{}, Tools: []string{}, Denied: []string{}}
			byName[workspace] = summary
		}

		return summary
	}

	for _, list := range lists {
		get(list.Workspace).Servers = append(get(list.Workspace).Servers, list.Servers...)
	}

	for _, entry := range entries {
		if !entry.Workspace.Valid {
			continue
		}

		summary := get(entry.Workspace.String)
		name := w.entryName(entry)

		switch {
		case entry.Effect == "deny":
			summary.Denied = append(summary.Denied, name)
		case entry.Tool == "":
			summary.Servers = append(summary.Servers, name)
		default:
			summary.Tools = append(summary.Tools, name)
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}

	slices.Sort(names)

	truncated := len(names) > MaxSummaryWorkspaces
	if truncated {
		names = names[:MaxSummaryWorkspaces]
	}

	result := make([]WorkspaceSummary, 0, len(names))
	for _, name := range names {
		summary := byName[name]
		summary.Servers = sortedUnique(summary.Servers)
		summary.Tools = sortedUnique(summary.Tools)
		summary.Denied = sortedUnique(summary.Denied)
		result = append(result, *summary)
	}

	return result, truncated
}

// sessionSummaries reads the principal's open sessions, newest first, at
// most MaxSummarySessions of them, with their additions and entries.
func (w *Workspaces) sessionSummaries(ctx context.Context, queries *db.Queries, principal string, idleTimeout time.Duration, entries []db.ScopeEntry) ([]SessionSummary, bool, error) {
	rows, err := queries.ListOpenSessionsWithServers(ctx, db.ListOpenSessionsWithServersParams{
		Principal:   principal,
		IdleTimeout: pgtype.Interval{Microseconds: idleTimeout.Microseconds(), Valid: true},
		// One more than the cap tells whether there were more.
		MaxRows: MaxSummarySessions + 1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("read open sessions: %w", err)
	}

	var (
		sessions  []SessionSummary
		pks       []int64
		truncated bool
	)

	index := map[int64]int{}

	for _, row := range rows {
		if _, found := index[row.ID]; found {
			continue
		}

		if len(sessions) == MaxSummarySessions {
			truncated = true

			break
		}

		index[row.ID] = len(sessions)
		pks = append(pks, row.ID)
		sessions = append(sessions, SessionSummary{
			Session: row.SessionID, Workspace: row.Workspace, Verified: row.Attestation == auth.AttestationAttested,
			LastSeenAt: row.LastSeenAt.Time.UTC(), Allowed: []string{}, Denied: []string{},
		})
	}

	if len(pks) == 0 {
		return []SessionSummary{}, truncated, nil
	}

	additions, err := queries.ListSessionAdditionsOfSessions(ctx, pks)
	if err != nil {
		return nil, false, fmt.Errorf("read session additions: %w", err)
	}

	for _, addition := range additions {
		if position, found := index[addition.SessionPk]; found {
			sessions[position].Allowed = append(sessions[position].Allowed, addition.Server)
		}
	}

	for _, entry := range entries {
		position, found := index[entry.SessionPk.Int64]
		if !entry.SessionPk.Valid || !found {
			continue
		}

		if entry.Effect == "deny" {
			sessions[position].Denied = append(sessions[position].Denied, w.entryName(entry))
		} else {
			sessions[position].Allowed = append(sessions[position].Allowed, w.entryName(entry))
		}
	}

	for position := range sessions {
		sessions[position].Allowed = sortedUnique(sessions[position].Allowed)
		sessions[position].Denied = sortedUnique(sessions[position].Denied)
	}

	return sessions, truncated, nil
}

// entryName is a scope entry as shown: the server, or the tool's proxied
// exposed name.
func (w *Workspaces) entryName(entry db.ScopeEntry) string {
	if entry.Tool == "" {
		return entry.Server
	}

	exposed := entry.Tool
	if template, found := w.catalog.Curation(entry.Server); found {
		exposed = toolcatalog.ExposedName(template, entry.Tool)
	}

	return ProxiedToolName(entry.Server, exposed)
}

func sortedUnique(values []string) []string {
	slices.Sort(values)

	return slices.Compact(values)
}
