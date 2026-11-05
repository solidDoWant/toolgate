package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Sessions page bounds.
const (
	// DecisionsShown is the number of tool.decision events on a session's page.
	DecisionsShown = 20
	// maxSessionsShown bounds the list of open sessions.
	maxSessionsShown = 500
)

// sessionRow is one open session as the pages show it.
type sessionRow struct {
	ID          int64
	Principal   string
	SessionID   string
	Workspace   string
	Attestation string
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

type sessionsPage struct {
	Sessions []sessionRow
	// ShowPrincipal is true for admins, who see every principal's rows.
	ShowPrincipal bool
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

func toSessionRow(row db.Session) sessionRow {
	return sessionRow{
		ID:          row.ID,
		Principal:   row.Principal,
		SessionID:   row.SessionID,
		Workspace:   row.Workspace,
		Attestation: row.Attestation,
		CreatedAt:   row.CreatedAt.Time.UTC(),
		LastSeenAt:  row.LastSeenAt.Time.UTC(),
	}
}

// sessions lists open sessions: the user's own, or every one for an admin.
func (u *UI) sessions(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	principal := session.Principal
	if session.Admin {
		principal = ""
	}

	rows, err := u.queries.ListOpenSessionsForUI(ctx, db.ListOpenSessionsForUIParams{
		Principal: principal,
		PageSize:  maxSessionsShown,
	})
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot list sessions for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the sessions. Try again later.", http.StatusInternalServerError)

		return
	}

	data := sessionsPage{ShowPrincipal: session.Admin, Sessions: make([]sessionRow, 0, len(rows))}
	for _, row := range rows {
		data.Sessions = append(data.Sessions, toSessionRow(row))
	}

	u.render(writer, request, "sessions", "Sessions", data)
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

	data := sessionPage{Session: toSessionRow(row), Servers: make([]string, 0, len(servers)), Decisions: make([]decisionRow, 0, len(events))}

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
