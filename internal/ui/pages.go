package ui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

// ServerSource gives the Servers page its rows. gateway.CatalogView
// implements it.
type ServerSource interface {
	// Servers returns the catalog as toolgate_list_servers shows it to
	// identity.
	Servers(ctx context.Context, identity auth.Identity) ([]gateway.ServerEntry, error)
	// LiveInstances returns principal's live instances and, with
	// includeShared, the shared ones.
	LiveInstances(ctx context.Context, principal string, includeShared bool) ([]gateway.InstanceEntry, error)
	// RequestRestart asks the reconciler to replace an outdated instance.
	// It returns gateway.ErrInstanceNotFound when the instance isn't
	// principal's to restart, and gateway.ErrInstanceChanged when it isn't
	// live, ready and outdated any more.
	RequestRestart(ctx context.Context, id, principal string, admin bool) error
}

//go:embed templates
var templateFiles embed.FS

// pageTemplates holds one template set per page, each with the layout.
var pageTemplates = map[string]*template.Template{
	"servers": template.Must(template.ParseFS(templateFiles, "templates/layout.html", "templates/servers.html")),
}

// page is what the layout renders around a page's content.
type page struct {
	Title     string
	User      string
	Admin     bool
	CSRFToken string
	Data      any
}

// render writes a page for the request's session.
func (u *UI) render(writer http.ResponseWriter, request *http.Request, name, title string, data any) {
	session, _ := SessionFrom(request.Context())

	user := session.Email
	if user == "" {
		user = session.Name
	}

	if user == "" {
		user = session.Principal
	}

	var body bytes.Buffer

	err := pageTemplates[name].ExecuteTemplate(&body, "layout", page{
		Title:     title,
		User:      user,
		Admin:     session.Admin,
		CSRFToken: session.CSRFTokenValue(),
		Data:      data,
	})
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot render a web UI page", "page", name, "error", err)
		http.Error(writer, "The gateway can't show this page. Try again later.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = writer.Write(body.Bytes())
}

type serversPage struct {
	Notice    string
	Servers   []gateway.ServerEntry
	Instances []gateway.InstanceEntry
}

// Notices the Servers page shows after a restart request, by the value of
// its `notice` parameter.
const (
	noticeRestartRequested = "restart-requested"
	noticeInstanceChanged  = "instance-changed"
)

var serversNotices = map[string]string{
	noticeRestartRequested: "The restart was requested. The new instance starts in a moment.",
	noticeInstanceChanged:  "The instance changed before the restart request arrived. The list below is current.",
}

// servers shows the catalog as toolgate_list_servers shows it to the user
// with `user` attestation and no workspace or session claim, and the user's
// live instances (admins: also the shared ones).
func (u *UI) servers(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	identity := auth.Identity{
		Principal:   session.Principal,
		Email:       session.Email,
		Groups:      session.Groups,
		Attestation: auth.AttestationUser,
	}

	servers, err := u.options.Servers.Servers(ctx, identity)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot list the catalog for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the catalog. Try again later.", http.StatusInternalServerError)

		return
	}

	instances, err := u.options.Servers.LiveInstances(ctx, session.Principal, session.Admin)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot list instances for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the instances. Try again later.", http.StatusInternalServerError)

		return
	}

	u.render(writer, request, "servers", "Servers", serversPage{
		Notice:    serversNotices[request.URL.Query().Get("notice")],
		Servers:   servers,
		Instances: instances,
	})
}

// restartInstance requests the restart of an outdated instance
// (docs/design/phase-2.md §6, "Restart"). Another user's instance, or a
// shared one for a non-admin, gives 404. If the instance isn't live, ready
// and outdated any more, the Servers page says so.
func (u *UI) restartInstance(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())

	err := u.options.Servers.RequestRestart(request.Context(), request.PathValue("id"), session.Principal, session.Admin)

	switch {
	case errors.Is(err, gateway.ErrInstanceNotFound):
		http.NotFound(writer, request)
	case errors.Is(err, gateway.ErrInstanceChanged):
		http.Redirect(writer, request, HomePath+"?notice="+noticeInstanceChanged, http.StatusSeeOther)
	case err != nil:
		u.logger.ErrorContext(request.Context(), "cannot request an instance restart", "error", err)
		http.Error(writer, "The gateway can't request the restart. Try again later.", http.StatusInternalServerError)
	default:
		http.Redirect(writer, request, HomePath+"?notice="+noticeRestartRequested, http.StatusSeeOther)
	}
}
