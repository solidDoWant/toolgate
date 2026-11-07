package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/principal"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Sessions page bounds.
const (
	// DecisionsShown is the number of tool.decision events on a session's page.
	DecisionsShown = 20
	// maxSessionsShown bounds the list of sessions.
	maxSessionsShown = 500
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
	// Links of the state filter, keeping the other filters.
	Links []filterLink
}

type userOption struct {
	Value string
	Text  string
}

type filterLink struct {
	Label   string
	URL     string
	Current bool
}

type decisionRow struct {
	Key        string
	OccurredAt time.Time
	Server     string
	Tool       string
	Outcome    string
}

type sessionPage struct {
	Session   sessionRow
	Servers   []string
	Decisions []decisionRow
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

	data.Links = filterLinks(data)

	u.render(writer, request, "sessions", "Sessions", data)
}

// filterLinks builds the links of the state filter. They keep the workspace
// and user choices.
func filterLinks(data sessionsPage) []filterLink {
	links := make([]filterLink, 0, 3)

	for _, each := range []struct{ value, label string }{
		{filterActive, "Active"}, {filterRecent, "Last 24 hours"}, {filterAll, "All"},
	} {
		values := url.Values{"state": {each.value}}
		if data.Workspace != "" {
			values.Set("workspace", data.Workspace)
		}

		if data.ShowPrincipal && data.User != "" {
			values.Set("user", data.User)
		}

		links = append(links, filterLink{Label: each.label, URL: "/ui/sessions?" + values.Encode(), Current: data.Filter == each.value})
	}

	return links
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

	servers, err := u.queries.ListSessionServers(ctx, id)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a session's servers for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the session. Try again later.", http.StatusInternalServerError)

		return
	}

	events, err := u.queries.ListRecentDecisions(ctx, db.ListRecentDecisionsParams{
		Principal: row.Principal,
		Session:   row.SessionID,
		Since:     row.CreatedAt,
		PageSize:  DecisionsShown,
	})
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a session's decisions for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the audit log. Try again later.", http.StatusInternalServerError)

		return
	}

	data := sessionPage{Session: toSessionRow(row, time.Now(), u.resumeWindow()), Servers: make([]string, 0, len(servers)), Decisions: make([]decisionRow, 0, len(events))}

	data.Session.Who = u.namesFor(ctx, session.Principal, row.Principal).show(row.Principal)

	for _, server := range servers {
		data.Servers = append(data.Servers, server.TemplateNs+"/"+server.TemplateName)
	}

	for _, event := range events {
		data.Decisions = append(data.Decisions, decisionRow{
			Key:        eventKey(event.OccurredAt.Time, event.ID),
			OccurredAt: event.OccurredAt.Time.UTC(),
			Server:     event.Server,
			Tool:       event.Tool,
			Outcome:    decisionOutcome(event.Details),
		})
	}

	u.render(writer, request, "session", "Session", data)
}

// decisionOutcome reads the decision ("allow" or "deny") from an event's details.
func decisionOutcome(details []byte) string {
	var parsed struct {
		Decision string `json:"decision"`
	}

	if err := json.Unmarshal(details, &parsed); err != nil {
		return ""
	}

	return parsed.Decision
}
