package ui

import (
	"bytes"
	"context"
	"embed"
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
}

//go:embed templates
var templateFiles embed.FS

// pageTemplates holds one template set per page, each with the layout.
var pageTemplates = map[string]*template.Template{
	"servers":     template.Must(template.ParseFS(templateFiles, "templates/layout.html", "templates/servers.html")),
	"audit":       template.Must(template.ParseFS(templateFiles, "templates/layout.html", "templates/audit.html")),
	"audit_event": template.Must(template.ParseFS(templateFiles, "templates/layout.html", "templates/audit_event.html")),
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
	Servers   []gateway.ServerEntry
	Instances []gateway.InstanceEntry
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

	u.render(writer, request, "servers", "Servers", serversPage{Servers: servers, Instances: instances})
}
