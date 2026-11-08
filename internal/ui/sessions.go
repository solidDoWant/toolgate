package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/principal"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Sessions page bounds.
const (
	// CallsShown is the number of call events on a session's page.
	CallsShown = 50
	// maxSessionsShown bounds the list of sessions.
	maxSessionsShown = 500
	// ActivityWindow is how far back the activity counts of the list look.
	ActivityWindow = time.Hour
)

// Session states and the filters of the list.
const (
	// ActiveWindow is how recent a request must be for a session to be
	// active. A session with no request for longer is idle.
	ActiveWindow = 5 * time.Minute
	// recentWindow is the "recent" filter.
	recentWindow = 24 * time.Hour
	// DefaultSessionResumeWindow is the sessionResumeWindow when Options
	// sets none.
	DefaultSessionResumeWindow = 24 * time.Hour

	// connectorPrefix starts the session ID of a connector session.
	connectorPrefix = "mcp."

	allUsers = "all"
)

const (
	stateActive    = "active"
	stateIdle      = "idle"
	stateEnded     = "ended"
	stateResumable = "resumable"

	filterActive = "active"
	filterRecent = "recent"
	filterAll    = "all"
)

// sessionState is the state of a session as the list shows it.
type sessionState struct {
	// Name is active, idle, ended or resumable.
	Name string
	// Until is, for a resumable session, when it can no longer be resumed.
	Until timeView
}

// stateOf works out the state of a session at now.
func stateOf(now time.Time, row db.Session, resumeWindow time.Duration) sessionState {
	if !row.EndedAt.Valid {
		if now.Sub(row.LastSeenAt.Time) < ActiveWindow {
			return sessionState{Name: stateActive}
		}

		return sessionState{Name: stateIdle}
	}

	until := row.EndedAt.Time.Add(resumeWindow)
	if !strings.HasPrefix(row.SessionID, connectorPrefix) && until.After(now) {
		return sessionState{Name: stateResumable, Until: relativeTime(now, until)}
	}

	return sessionState{Name: stateEnded}
}

// sessionRow is one session as the pages show it.
type sessionRow struct {
	ID        int64
	Principal string
	// Who is Principal as people read it.
	Who       principal.Display
	SessionID string
	// ShortID is SessionID shortened for the table.
	ShortID        string
	Workspace      string
	WorkspaceLabel workspaceLabel
	Attestation    string
	// Verified is true for an attested workspace.
	Verified bool
	// Client is the client name and version, for example "Claude Code 2.1.319".
	Client string
	// Connector is true for a connector session, false for a helper session.
	Connector  bool
	ToolSearch bool
	State      sessionState
	CreatedAt  time.Time
	LastSeenAt time.Time
	// Servers are the enabled servers, as namespace/name. The list page fills
	// Servers and Activity.
	Servers  []string
	Activity sessionActivity
}

// sessionActivity is the counts the list shows for a session.
type sessionActivity struct {
	RunningInstances int64
	// ToolCalls and Refusals count the last ActivityWindow.
	ToolCalls int64
	Refusals  int64
	// PendingApprovals counts the approvals that wait for a decision.
	PendingApprovals int64
	// AuditURL lists the session's audit events of the last ActivityWindow.
	AuditURL string
}

// auditLinkFor links to the audit page filtered to one session and its
// principal, from the start of the activity window.
func auditLinkFor(row sessionRow, now time.Time) string {
	values := url.Values{
		"principal": {row.Principal},
		"session":   {row.SessionID},
		"from":      {now.Add(-ActivityWindow).UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)},
	}

	return "/ui/audit?" + values.Encode()
}

type sessionsPage struct {
	Sessions []sessionRow
	// ShowPrincipal is true for admins, who may filter by user.
	ShowPrincipal bool
	// Filters, as chosen.
	Filter    string
	Workspace string
	User      string
	// Workspaces and Users fill the filter lists.
	Workspaces []workspaceLabel
	Users      []userOption
	// Links are the choices of the state filter.
	Links []filterLink
}

type userOption struct {
	Value string
	Text  string
}

type filterLink struct {
	Label   string
	Value   string
	Current bool
}

// callRow is one call event on the session page.
type callRow struct {
	Key        string
	OccurredAt time.Time
	Server     string
	Tool       string
	// Outcome is the word for the outcome badge.
	Outcome string
	// Reason is why a call was refused or denied.
	Reason string
	// LatencyMS is the upstream duration of a result, or -1 when unknown.
	LatencyMS int64
}

// sessionServerRow is one enabled server of the session page.
type sessionServerRow struct {
	// Template is namespace/name of the template.
	Template string
	Name     string
	// Namespace is the namespace of the template.
	Namespace string
	Status    string
	Reason    string
	Remote    bool
	// Denied is true when the session's own scope denies the whole server.
	Denied bool
}

// scopeView is what a session may use, from the workspace, the defaults and
// the session's own additions.
type scopeView struct {
	// Known is false when the scope can't be shown (workspace lists are off).
	Known bool
	// Verified is true for a session in an attested workspace.
	Verified bool
	// Base says where the base list comes from, in words.
	Base string
	// BaseServers are the servers of the list that applies, when it is a list.
	BaseServers []string
	// WorkspaceAllowed and WorkspaceDenied are the workspace's scope entries.
	WorkspaceAllowed, WorkspaceDenied []string
	// SessionAdded are servers added for this session only; SessionAllowed
	// and SessionDenied are its scope entries.
	SessionAdded                  []string
	SessionAllowed, SessionDenied []string
}

type sessionPage struct {
	Session  sessionRow
	Servers  []sessionServerRow
	Scope    scopeView
	Calls    []callRow
	AuditURL string
	// CanDeny is true for the session's owner when the gateway can change
	// scopes.
	CanDeny bool
	// DenyPath is the confirmation page of a deny.
	DenyPath string
}

func clientText(name, version string) string {
	return strings.TrimSpace(name + " " + version)
}

func toSessionRow(row db.Session, now time.Time, resumeWindow time.Duration) sessionRow {
	return sessionRow{
		ID:             row.ID,
		Principal:      row.Principal,
		SessionID:      row.SessionID,
		ShortID:        principal.Short(row.SessionID),
		Workspace:      row.Workspace,
		WorkspaceLabel: labelWorkspace(row.Workspace),
		Attestation:    row.Attestation,
		Verified:       row.Attestation == "attested",
		Connector:      strings.HasPrefix(row.SessionID, connectorPrefix),
		ToolSearch:     row.ToolSearch,
		State:          stateOf(now, row, resumeWindow),
		CreatedAt:      row.CreatedAt.Time.UTC(),
		LastSeenAt:     row.LastSeenAt.Time.UTC(),
	}
}

func (u *UI) resumeWindow() time.Duration {
	if u.options.SessionResumeWindow > 0 {
		return u.options.SessionResumeWindow
	}

	return DefaultSessionResumeWindow
}

// sessions lists sessions: the user's own, or for an admin also those of one
// or all other users. The state, workspace and user filters are in the URL.
func (u *UI) sessions(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()
	now := time.Now()
	window := u.resumeWindow()
	query := request.URL.Query()

	filter := query.Get("state")
	if filter != filterRecent && filter != filterAll {
		filter = filterActive
	}

	data := sessionsPage{ShowPrincipal: session.Admin, Filter: filter, Workspace: query.Get("workspace")}

	// Only an admin may look at someone else. Anyone else gets their own.
	owner := session.Principal

	if session.Admin {
		data.User = query.Get("user")

		switch data.User {
		case "":
			data.User = session.Principal
		case allUsers:
			owner = ""
		default:
			owner = data.User
		}
	}

	endedAfter := pgtype.Timestamptz{Time: now.Add(-window), Valid: true}
	seenAfter := pgtype.Timestamptz{Time: now.Add(-window), Valid: true}

	switch filter {
	case filterActive:
		seenAfter.Time = now.Add(-ActiveWindow)
	case filterRecent:
		seenAfter.Time = now.Add(-recentWindow)
	}

	rows, err := u.queries.ListSessionsForUI(ctx, db.ListSessionsForUIParams{
		OnlyOpen:   filter == filterActive,
		EndedAfter: endedAfter,
		Principal:  owner,
		Workspace:  data.Workspace,
		SeenAfter:  seenAfter,
		PageSize:   maxSessionsShown,
	})
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot list sessions for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the sessions. Try again later.", http.StatusInternalServerError)

		return
	}

	data.Sessions = make([]sessionRow, 0, len(rows))
	principals := make([]string, 0, len(rows))

	for _, row := range rows {
		each := toSessionRow(db.Session{
			ID: row.ID, Principal: row.Principal, SessionID: row.SessionID, Workspace: row.Workspace,
			Attestation: row.Attestation, CreatedAt: row.CreatedAt, LastSeenAt: row.LastSeenAt,
			EndedAt: row.EndedAt, ToolSearch: row.ToolSearch,
		}, now, window)
		each.Client = clientText(row.ClientName, row.ClientVersion)
		data.Sessions = append(data.Sessions, each)
		principals = append(principals, row.Principal)
	}

	u.fillSessionActivity(ctx, data.Sessions, now)

	workspaces, err := u.queries.ListSessionWorkspaces(ctx, db.ListSessionWorkspacesParams{EndedAfter: endedAfter, Principal: owner})
	if err != nil {
		u.logger.WarnContext(ctx, "cannot list session workspaces for the web UI", "error", err)
	}

	for _, workspace := range workspaces {
		data.Workspaces = append(data.Workspaces, labelWorkspace(workspace))
	}

	var others []string

	if session.Admin {
		others, err = u.queries.ListSessionPrincipals(ctx, endedAfter)
		if err != nil {
			u.logger.WarnContext(ctx, "cannot list session users for the web UI", "error", err)
		}
	}

	names := u.namesFor(ctx, session.Principal, append(principals, others...)...)
	for index := range data.Sessions {
		data.Sessions[index].Who = names.show(data.Sessions[index].Principal)
	}

	for _, other := range others {
		data.Users = append(data.Users, userOption{Value: other, Text: names.show(other).Text})
	}

	data.Links = stateOptions(data)

	u.render(writer, request, "sessions", "Sessions", data)
}

// fillSessionActivity adds the servers and activity counts to the listed
// sessions with two queries for the whole page. A failed query leaves the
// page without those columns' data and is logged.
func (u *UI) fillSessionActivity(ctx context.Context, sessions []sessionRow, now time.Time) {
	if len(sessions) == 0 {
		return
	}

	ids := make([]int64, 0, len(sessions))
	positions := make(map[int64]int, len(sessions))

	for index, each := range sessions {
		ids = append(ids, each.ID)
		positions[each.ID] = index
		sessions[index].Activity.AuditURL = auditLinkFor(each, now)
	}

	servers, err := u.queries.ListServersForSessions(ctx, ids)
	if err != nil {
		u.logger.WarnContext(ctx, "cannot list session servers for the web UI", "error", err)
	}

	for _, server := range servers {
		index := positions[server.SessionPk]
		sessions[index].Servers = append(sessions[index].Servers, server.TemplateNs+"/"+server.TemplateName)
	}

	counts, err := u.queries.CountSessionActivity(ctx, db.CountSessionActivityParams{
		Since:      pgtype.Timestamptz{Time: now.Add(-ActivityWindow), Valid: true},
		SessionPks: ids,
	})
	if err != nil {
		u.logger.WarnContext(ctx, "cannot count session activity for the web UI", "error", err)
	}

	for _, count := range counts {
		activity := &sessions[positions[count.SessionPk]].Activity
		activity.RunningInstances = count.RunningInstances
		activity.ToolCalls = count.ToolCalls
		activity.Refusals = count.Refusals
		activity.PendingApprovals = count.PendingApprovals
	}
}

// stateOptions lists the choices of the state filter, marking the current one.
func stateOptions(data sessionsPage) []filterLink {
	options := make([]filterLink, 0, 3)

	for _, each := range []struct{ value, label string }{
		{filterActive, "Active"}, {filterRecent, "Last 24 hours"}, {filterAll, "All"},
	} {
		options = append(options, filterLink{Label: each.label, Value: each.value, Current: data.Filter == each.value})
	}

	return options
}

// sessionPageHandler shows one open session to its principal or an admin.
// Anyone else, and an ended or unknown session, gets 404.
func (u *UI) sessionPageHandler(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		http.NotFound(writer, request)

		return
	}

	row, err := u.queries.GetSessionByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.EndedAt.Valid || (!session.Admin && row.Principal != session.Principal))) {
		http.NotFound(writer, request)

		return
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a session for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the session. Try again later.", http.StatusInternalServerError)

		return
	}

	data, err := u.buildSessionPage(ctx, session, row)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a session's details for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the session. Try again later.", http.StatusInternalServerError)

		return
	}

	u.render(writer, request, "session", "Session", data)
}

func (u *UI) buildSessionPage(ctx context.Context, viewer Session, row db.Session) (sessionPage, error) {
	data := sessionPage{
		Session:  toSessionRow(row, time.Now(), u.resumeWindow()),
		AuditURL: "/ui/audit?" + url.Values{"session": {row.SessionID}}.Encode(),
	}

	data.Session.Who = u.namesFor(ctx, viewer.Principal, row.Principal).show(row.Principal)

	client, err := u.queries.GetSessionClient(ctx, row.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return sessionPage{}, fmt.Errorf("read client: %w", err)
	}

	data.Session.Client = clientText(client.ClientName, client.ClientVersion)

	if data.Servers, err = u.sessionServers(ctx, viewer, row); err != nil {
		return sessionPage{}, err
	}

	if data.Scope, err = u.sessionScope(ctx, row); err != nil {
		return sessionPage{}, err
	}

	data.DenyPath = sessionDenyPath(row.ID)

	data.CanDeny = u.options.Workspaces != nil && viewer.Principal == row.Principal
	for index := range data.Servers {
		server := &data.Servers[index]
		server.Denied = slices.Contains(data.Scope.SessionDenied, server.Name)
	}

	events, err := u.queries.ListSessionCallEvents(ctx, db.ListSessionCallEventsParams{
		Principal: row.Principal,
		Session:   row.SessionID,
		Since:     row.CreatedAt,
		PageSize:  CallsShown,
	})
	if err != nil {
		return sessionPage{}, fmt.Errorf("read calls: %w", err)
	}

	data.Calls = make([]callRow, 0, len(events))

	for _, event := range events {
		data.Calls = append(data.Calls, callRowOf(event))
	}

	return data, nil
}

// sessionServers lists the session's enabled servers with the status the
// session's own identity would see.
func (u *UI) sessionServers(ctx context.Context, viewer Session, row db.Session) ([]sessionServerRow, error) {
	enabled, err := u.queries.ListSessionServers(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("read servers: %w", err)
	}

	servers := make([]sessionServerRow, 0, len(enabled))
	if len(enabled) == 0 {
		return servers, nil
	}

	known := map[string]gateway.ServerInfo{}

	if u.options.Servers != nil {
		identity := auth.Identity{
			Principal: row.Principal, Kind: auth.KindUser, Attestation: row.Attestation, Workspace: row.Workspace,
		}
		// Only the owner's groups are known; they decide catalog access.
		if viewer.Principal == row.Principal {
			identity.Email, identity.Groups = viewer.Email, viewer.Groups
		}

		infos, err := u.options.Servers.Catalog(ctx, identity)
		if err != nil {
			return nil, fmt.Errorf("read the catalog: %w", err)
		}

		for _, info := range infos {
			known[info.Name] = info
		}
	}

	for _, server := range enabled {
		each := sessionServerRow{Template: server.TemplateNs + "/" + server.TemplateName, Name: server.TemplateName, Namespace: server.TemplateNs, Status: "unknown"}
		if info, found := known[server.TemplateName]; found {
			each.Status, each.Reason, each.Remote = info.Status, info.Reason, info.Remote
		}

		servers = append(servers, each)
	}

	return servers, nil
}

// sessionScope reads what the session's workspace, the owner's defaults and
// the session's additions allow and deny.
func (u *UI) sessionScope(ctx context.Context, row db.Session) (scopeView, error) {
	if u.options.Workspaces == nil {
		return scopeView{}, nil
	}

	scopes, err := u.options.Workspaces.Scopes(ctx, row.Principal)
	if err != nil {
		return scopeView{}, fmt.Errorf("read workspace lists: %w", err)
	}

	view := scopeView{Known: true, Verified: row.Attestation == "attested" && row.Workspace != ""}

	if view.Verified {
		view.Base = "the default for verified workspaces: " + scopeDefaultText(scopes.VerifiedDefault)

		for _, list := range scopes.Lists {
			if list.Workspace == row.Workspace {
				view.Base, view.BaseServers = "the list of this workspace", list.Servers
			}
		}

		entries := scopes.Entries[row.Workspace]
		view.WorkspaceAllowed, view.WorkspaceDenied = entries.Allowed, entries.Denied
	} else {
		view.Base = "the default for sessions without a verified workspace: " + scopeDefaultText(scopes.UnverifiedDefault)
		if scopes.UnverifiedDefault == gateway.ScopeList {
			view.BaseServers = scopes.UnverifiedServers
		}
	}

	if view.SessionAdded, err = u.queries.ListSessionAdditions(ctx, db.ListSessionAdditionsParams{Principal: row.Principal, SessionID: row.SessionID}); err != nil {
		return scopeView{}, fmt.Errorf("read session additions: %w", err)
	}

	entries, err := u.options.Workspaces.SessionEntries(ctx, []int64{row.ID})
	if err != nil {
		return scopeView{}, fmt.Errorf("read session scope entries: %w", err)
	}

	view.SessionAllowed, view.SessionDenied = entries[row.ID].Allowed, entries[row.ID].Denied

	return view, nil
}

func scopeDefaultText(value string) string {
	switch value {
	case gateway.ScopeAll:
		return "all servers"
	case gateway.ScopeList:
		return "a list of servers"
	default:
		return "no servers"
	}
}

// callRowOf maps a call event to a row. A decision shows its decision, a
// result its outcome with the latency, and an approval event its state.
func callRowOf(event db.ListSessionCallEventsRow) callRow {
	var details struct {
		Decision   string `json:"decision"`
		Outcome    string `json:"outcome"`
		Reason     string `json:"reason"`
		Approval   string `json:"approval"`
		DurationMS *int64 `json:"duration_ms"`
	}

	_ = json.Unmarshal(event.Details, &details)

	row := callRow{
		Key:        eventKey(event.OccurredAt.Time, event.ID),
		OccurredAt: event.OccurredAt.Time.UTC(),
		Server:     event.Server,
		Tool:       event.Tool,
		Reason:     details.Reason,
		LatencyMS:  -1,
	}

	switch event.Type {
	case "tool.result":
		row.Outcome = details.Outcome
		if details.DurationMS != nil {
			row.LatencyMS = *details.DurationMS
		}
	case "approval.request":
		row.Outcome = "pending"
	case "approval.expire":
		row.Outcome = "expired"
	default:
		switch {
		case details.Decision == "allow" && details.Approval != "":
			row.Outcome = "approved"
		case details.Decision == "refuse":
			row.Outcome = "refused"
		default:
			row.Outcome = details.Decision
		}
	}

	return row
}
