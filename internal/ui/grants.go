package ui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Grants page paths.
const (
	GrantsPath    = "/ui/grants"
	NewGrantPath  = "/ui/grants/new"
	defaultGrant  = "1h"
	htmxHeader    = "HX-Request"
	grantRowBlock = "grant_row"
)

// grantDurations are the durations the grant form offers. The POST accepts
// any Go duration from 1 minute to 7 days.
var grantDurations = []grantDuration{
	{Value: "1m", Label: "1 minute"},
	{Value: "15m", Label: "15 minutes"},
	{Value: "1h", Label: "1 hour"},
	{Value: "8h", Label: "8 hours"},
	{Value: "24h", Label: "1 day"},
	{Value: "168h", Label: "7 days"},
}

type grantDuration struct{ Value, Label string }

// GrantSource creates, revokes and lists the user's grants.
// gateway.Grants implements it.
type GrantSource interface {
	// Servers returns the servers identity can create grants for.
	Servers(identity auth.Identity) []gateway.GrantableServer
	// OpenSessions returns the X-Toolgate-Session values of principal's
	// open sessions.
	OpenSessions(ctx context.Context, principal string) ([]string, error)
	// List returns principal's live and recent grants, newest first.
	List(ctx context.Context, principal string) ([]db.Grant, error)
	// Create checks and inserts a grant for identity. A refused request
	// returns a *gateway.GrantRefusal.
	Create(ctx context.Context, identity auth.Identity, request gateway.GrantRequest) (db.Grant, error)
	// Revoke revokes principal's own live grant, or returns
	// gateway.ErrGrantNotFound.
	Revoke(ctx context.Context, principal, id string) (db.Grant, error)
	// Get returns principal's own grant, or gateway.ErrGrantNotFound.
	Get(ctx context.Context, principal, id string) (db.Grant, error)
}

func (u *UI) registerGrants() {
	u.Handle("GET "+GrantsPath, User, http.HandlerFunc(u.grants))
	u.Handle("GET "+NewGrantPath, User, http.HandlerFunc(u.newGrant))
	u.Handle("POST "+GrantsPath, User, RequireStepUp(grantFormPath, http.HandlerFunc(u.createGrant)))
	u.Handle("POST "+GrantsPath+"/{id}/revoke", User, http.HandlerFunc(u.revokeGrant))
}

// GrantLink returns the function that builds the link of a policy refusal
// that needs a grant: the grant form at publicURL, filled with the server,
// the tool and the session (docs/design/phase-2.md §4, "Links").
func GrantLink(publicURL string) func(server, tool, session string) string {
	base := strings.TrimSuffix(publicURL, "/") + NewGrantPath

	return func(server, tool, session string) string {
		query := url.Values{"server": {server}}
		if tool != "" {
			query.Set("tool", tool)
		}

		if session != "" {
			query.Set("session", session)
		}

		return base + "?" + query.Encode()
	}
}

// grantFormPath is the grant form filled with a POST's server, tool and
// session, where a step-up returns to.
func grantFormPath(request *http.Request) string {
	query := url.Values{}

	for _, field := range []string{"server", "tool", "session"} {
		if value := request.PostFormValue(field); value != "" {
			query.Set(field, value)
		}
	}

	if len(query) == 0 {
		return NewGrantPath
	}

	return NewGrantPath + "?" + query.Encode()
}

// grantRow is one row of the grants table.
type grantRow struct {
	ID, Server, Tool, Session string
	CreatedAt, ExpiresAt      time.Time
	RevokedAt                 *time.Time
	Status                    string
	Live                      bool
	CSRFToken                 string
}

func newGrantRow(grant db.Grant, csrfToken string, now time.Time) grantRow {
	row := grantRow{
		ID:        grant.ID,
		Server:    grant.TemplateName,
		Tool:      grant.Tool,
		Session:   grant.SessionID.String,
		CreatedAt: grant.CreatedAt.Time,
		ExpiresAt: grant.ExpiresAt.Time,
		Status:    "live",
		Live:      true,
		CSRFToken: csrfToken,
	}

	switch {
	case grant.RevokedAt.Valid:
		revokedAt := grant.RevokedAt.Time
		row.RevokedAt = &revokedAt
		row.Status, row.Live = "revoked", false
	case !grant.ExpiresAt.Time.After(now):
		row.Status, row.Live = "expired", false
	}

	return row
}

// grants lists the user's live and recent grants.
func (u *UI) grants(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())

	grants, err := u.options.Grants.List(request.Context(), session.Principal)
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot list grants for the web UI", "error", err)
		http.Error(writer, "The gateway can't read your grants. Try again later.", http.StatusInternalServerError)

		return
	}

	now := time.Now()
	rows := make([]grantRow, 0, len(grants))

	for _, grant := range grants {
		rows = append(rows, newGrantRow(grant, session.CSRFTokenValue(), now))
	}

	u.render(writer, request, "grants", "Grants", rows)
}

// grantForm is the grant form's data.
type grantForm struct {
	Servers   []gateway.GrantableServer
	Sessions  []string
	Durations []grantDuration
	// Server, Tool, Session and Duration are the values filled in.
	Server, Tool, Session, Duration string
	Error                           string
}

// newGrant shows the grant form, filled from ?server=&tool=&session=, or
// the step-up prompt when the sign-in is too old to submit it.
func (u *UI) newGrant(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	if StepUpNeeded(session) {
		WriteStepUpPrompt(writer, request)

		return
	}

	query := request.URL.Query()
	u.renderGrantForm(writer, request, http.StatusOK, grantForm{
		Server:   query.Get("server"),
		Tool:     query.Get("tool"),
		Session:  query.Get("session"),
		Duration: defaultGrant,
	})
}

func (u *UI) renderGrantForm(writer http.ResponseWriter, request *http.Request, status int, form grantForm) {
	session, _ := SessionFrom(request.Context())

	sessions, err := u.options.Grants.OpenSessions(request.Context(), session.Principal)
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot list sessions for the grant form", "error", err)
		http.Error(writer, "The gateway can't read your sessions. Try again later.", http.StatusInternalServerError)

		return
	}

	form.Servers = u.options.Grants.Servers(sessionIdentity(session))
	form.Sessions = sessions
	form.Durations = grantDurations

	if status != http.StatusOK {
		writer.WriteHeader(status)
	}

	u.render(writer, request, "grant_new", "New grant", form)
}

// createGrant creates a grant for the user. RequireStepUp has checked the
// sign-in's age.
func (u *UI) createGrant(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())

	form := grantForm{
		Server:   request.PostFormValue("server"),
		Tool:     request.PostFormValue("tool"),
		Session:  request.PostFormValue("session"),
		Duration: request.PostFormValue("duration"),
	}

	lifetime, err := time.ParseDuration(form.Duration)
	if err != nil {
		form.Error = "Choose how long the grant lasts."
		u.renderGrantForm(writer, request, http.StatusBadRequest, form)

		return
	}

	_, err = u.options.Grants.Create(request.Context(), sessionIdentity(session), gateway.GrantRequest{
		Server:   form.Server,
		Tool:     form.Tool,
		Session:  form.Session,
		Lifetime: lifetime,
	})

	var refusal *gateway.GrantRefusal
	if errors.As(err, &refusal) {
		form.Error = refusal.Message
		u.renderGrantForm(writer, request, http.StatusBadRequest, form)

		return
	}

	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot create a grant", "error", err)
		http.Error(writer, "The gateway can't create the grant. Try again later.", http.StatusInternalServerError)

		return
	}

	http.Redirect(writer, request, GrantsPath, http.StatusSeeOther)
}

// revokeGrant revokes one of the user's own grants. No step-up: a revoke
// only narrows. Another user's grant gives 404, for admins too. An htmx
// request gets the revoked row; a plain form is redirected to the list.
func (u *UI) revokeGrant(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()
	id := request.PathValue("id")

	grant, err := u.options.Grants.Revoke(ctx, session.Principal, id)
	if errors.Is(err, gateway.ErrGrantNotFound) {
		// The user's own grant that is already revoked is shown as it is.
		grant, err = u.options.Grants.Get(ctx, session.Principal, id)
		if errors.Is(err, gateway.ErrGrantNotFound) {
			http.NotFound(writer, request)

			return
		}
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot revoke a grant", "error", err)
		http.Error(writer, "The gateway can't revoke the grant. Try again later.", http.StatusInternalServerError)

		return
	}

	if request.Header.Get(htmxHeader) != "true" {
		http.Redirect(writer, request, GrantsPath, http.StatusSeeOther)

		return
	}

	var body bytes.Buffer
	if err := pageTemplates["grants"].ExecuteTemplate(&body, grantRowBlock, newGrantRow(grant, session.CSRFTokenValue(), time.Now())); err != nil {
		u.logger.ErrorContext(ctx, "cannot render a grant row", "error", err)
		http.Error(writer, "The gateway can't show the grant. Reload the page.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = writer.Write(body.Bytes())
}

// sessionIdentity is the UI session as a signed-in user's identity, with no
// workspace or session claim.
func sessionIdentity(session Session) auth.Identity {
	return auth.Identity{
		Principal:   session.Principal,
		Email:       session.Email,
		Groups:      session.Groups,
		Attestation: auth.AttestationUser,
	}
}
