package ui

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// sessionDenyPath is the path of a session's deny confirmation page and form.
func sessionDenyPath(id int64) string {
	return sessionPath(id) + "/deny"
}

func sessionPath(id int64) string {
	return "/ui/sessions/" + strconv.FormatInt(id, 10)
}

func (u *UI) registerSessionDeny() {
	u.Handle("GET /ui/sessions/{id}/deny", User, http.HandlerFunc(u.sessionDenyConfirm))
	u.Handle("POST /ui/sessions/{id}/deny", User, http.HandlerFunc(u.sessionDeny))
}

type sessionDenyPage struct {
	Session sessionRow
	Server  string
	// Action is where the confirmation form posts.
	Action string
	// Back is the session page.
	Back string
}

// ownedSessionServer loads the open session of the path that belongs to the
// signed-in user, and the server of the request that the session has
// enabled. It answers 404 and returns false for anything else, including an
// admin who is not the owner: a deny is the owner's choice.
func (u *UI) ownedSessionServer(writer http.ResponseWriter, request *http.Request) (db.Session, string, bool) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		http.NotFound(writer, request)

		return db.Session{}, "", false
	}

	row, err := u.queries.GetSessionByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.EndedAt.Valid || row.Principal != session.Principal)) {
		http.NotFound(writer, request)

		return db.Session{}, "", false
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a session for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the session. Try again later.", http.StatusInternalServerError)

		return db.Session{}, "", false
	}

	// The form value is read from the body of a POST and the query of a GET.
	server := request.FormValue("server")

	enabled, err := u.queries.ListSessionServers(ctx, row.ID)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a session's servers for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the session. Try again later.", http.StatusInternalServerError)

		return db.Session{}, "", false
	}

	if !slices.ContainsFunc(enabled, func(each db.SessionServer) bool { return each.TemplateName == server }) {
		http.NotFound(writer, request)

		return db.Session{}, "", false
	}

	return row, server, true
}

// sessionDenyConfirm asks the owner to confirm denying one server.
func (u *UI) sessionDenyConfirm(writer http.ResponseWriter, request *http.Request) {
	row, server, ok := u.ownedSessionServer(writer, request)
	if !ok {
		return
	}

	data := sessionDenyPage{
		Session: toSessionRow(row, time.Now(), u.resumeWindow()),
		Server:  server,
		Action:  sessionDenyPath(row.ID),
		Back:    sessionPath(row.ID),
	}

	u.render(writer, request, "session_deny", "Deny a server", data)
}

// sessionDeny denies the server for this session only, with the same change
// and audit event as `toolgate workspace deny --session`. A deny narrows
// access, so it needs no fresh sign-in.
func (u *UI) sessionDeny(writer http.ResponseWriter, request *http.Request) {
	row, server, ok := u.ownedSessionServer(writer, request)
	if !ok {
		return
	}

	session, _ := SessionFrom(request.Context())

	err := u.options.Workspaces.ApplyNarrowing(request.Context(), sessionIdentity(session),
		gateway.WorkspaceChange{Action: gateway.WorkspaceDeny, Session: row.SessionID, Servers: []string{server}}, gateway.ViaUI)

	var refusal *gateway.WorkspaceError

	switch {
	case errors.As(err, &refusal):
		http.Error(writer, refusal.Message, refusal.Status)
	case err != nil:
		u.logger.ErrorContext(request.Context(), "cannot deny a server for a session", "error", err)
		http.Error(writer, "The gateway can't save the change. Try again later.", http.StatusInternalServerError)
	default:
		http.Redirect(writer, request, sessionPath(row.ID), http.StatusSeeOther)
	}
}
