package ui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strings"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/principal"
)

// ServerSource gives the Servers page its rows. gateway.CatalogView
// implements it.
type ServerSource interface {
	// Servers returns the catalog as toolgate_list_servers shows it to
	// identity.
	Servers(ctx context.Context, identity auth.Identity) ([]gateway.ServerEntry, error)
	// Catalog is Servers with the details the page shows.
	Catalog(ctx context.Context, identity auth.Identity) ([]gateway.ServerInfo, error)
	// ServerDetail returns one server and the tools identity may see in it.
	// found is false when the server doesn't exist or identity has no access
	// to it.
	ServerDetail(ctx context.Context, identity auth.Identity, name string) (gateway.ServerInfo, []gateway.ToolInfo, bool, error)
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
	"servers":        parsePage("servers"),
	"server":         parsePage("server"),
	"audit":          parsePage("audit"),
	"blocks":         parsePage("blocks", "block_form"),
	"sessions":       parsePage("sessions"),
	"session":        parsePage("session"),
	"session_deny":   parsePage("session_deny"),
	"audit_event":    parsePage("audit_event"),
	"grants":         parsePage("grants", "grant_form"),
	"grant_new":      parsePage("grant_new", "grant_form"),
	"approvals":      parsePage("approvals"),
	"approval":       parsePage("approval"),
	"workspaces":     parsePage("workspaces"),
	"access_request": parsePage("access_request"),
	"setup":          parsePage("setup"),
	"signin_failed":  parsePage("signin_failed"),
	"block_new":      parsePage("block_new", "block_form"),
	"secrets":        parsePage("secrets", "secret_form"),
	"secret_new":     parsePage("secret_new", "secret_form"),
}

// parsePage parses the layout, the shared components, one page's content
// template and the partials it shares with other pages.
func parsePage(file string, partials ...string) *template.Template {
	files := []string{"templates/layout.html", "templates/components.html", "templates/" + file + ".html"}
	for _, partial := range partials {
		files = append(files, "templates/"+partial+".html")
	}

	return template.Must(template.New(file).Funcs(template.FuncMap{
		"badge": badgeClass, "environmentBadge": environmentBadge, "environmentHelp": environmentHelp, "scopeHelp": scopeHelp,
		"workspace": labelWorkspace, "since": timeSince, "until": timeUntil, "outcome": outcomeBadge,
		"dict": dict, "list": list, "navLink": newNavLink, "short": principal.Short,
	}).ParseFS(templateFiles, files...))
}

// dict builds the argument of a shared component from key and value pairs.
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, errors.New("dict needs key and value pairs")
	}

	values := make(map[string]any, len(pairs)/2)

	for index := 0; index < len(pairs); index += 2 {
		key, ok := pairs[index].(string)
		if !ok {
			return nil, errors.New("dict keys are strings")
		}

		values[key] = pairs[index+1]
	}

	return values, nil
}

// list builds a list argument, such as a table's column headings.
func list(items ...any) []any {
	return items
}

// navLink is one link of the shell's sidebar.
type navLink struct {
	Href, Label string
	Current     bool
	// Count is the number on the link's badge; 0 shows none.
	Count int
}

func newNavLink(section, name, href, label string, count int) navLink {
	return navLink{Href: href, Label: label, Current: section == name, Count: count}
}

// badgeClass maps a status word to the colour class of its badge. Words it
// doesn't know get the neutral class.
func badgeClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "running", "live", "approved", "allow", "allowed", "attested", "verified", "valid", "current":
		return "badge-ok"
	case "starting", "pending", "outdated", "stopping":
		return "badge-warn"
	case "blocked", "failed", "denied", "deny", "expired", "revoked", "invalid", "error":
		return "badge-bad"
	default:
		return "badge-neutral"
	}
}

// environmentBadge is the badge class of an access.environments value.
func environmentBadge(environments string) string {
	switch environments {
	case "any":
		return "badge-ok"
	case "attested":
		return "badge-warn"
	default:
		return "badge-neutral"
	}
}

// environmentHelp says in plain words what an access.environments value
// means for the user.
func environmentHelp(environments string) string {
	switch environments {
	case "any":
		return "Works from any signed-in machine."
	case "attested":
		return "Needs an attested workspace."
	default:
		return "Needs an attested workspace or a grant."
	}
}

// scopeHelp says in plain words who shares an instance of a server.
func scopeHelp(scope string) string {
	switch scope {
	case "per-session":
		return "Each session gets its own instance."
	case "per-user":
		return "All your sessions share one instance."
	case "shared":
		return "Everyone shares one instance."
	default:
		return ""
	}
}

// navSection names the nav link a page highlights.
func navSection(name string) string {
	switch name {
	case "server":
		return "servers"
	case "session", "session_deny":
		return "sessions"
	case "audit_event":
		return "audit"
	case "grant_new":
		return "grants"
	case "block_new":
		return "blocks"
	case "secret_new":
		return "secrets"
	case "approval", "access_request":
		return "approvals"
	default:
		return name
	}
}

// page is what the layout renders around a page's content.
type page struct {
	Title string
	// Section is the nav link to highlight.
	Section string
	User    string
	Admin   bool
	// Secrets is true when the Secrets page is registered, so the nav links to it.
	Secrets   bool
	CSRFToken string
	// SignedOut leaves out the user, the sign-out button and the nav, for a
	// page shown to a browser without a session.
	SignedOut bool
	// Approvals is the number of the user's pending approvals, for the
	// sidebar's badge.
	Approvals int
	Data      any
}

// render writes a page for the request's session.
func (u *UI) render(writer http.ResponseWriter, request *http.Request, name, title string, data any) {
	u.renderStatus(writer, request, http.StatusOK, name, title, data)
}

// renderStatus is render with an HTTP status.
func (u *UI) renderStatus(writer http.ResponseWriter, request *http.Request, status int, name, title string, data any) {
	session, _ := SessionFrom(request.Context())

	user := session.Email
	if user == "" {
		user = session.Name
	}

	if user == "" {
		user = session.Principal
	}

	u.writePage(writer, request, status, name, page{
		Title:     title,
		Section:   navSection(name),
		User:      user,
		Admin:     session.Admin,
		Secrets:   u.options.Secrets != nil,
		CSRFToken: session.CSRFTokenValue(),
		Approvals: u.pendingApprovals(request.Context(), session.Principal),
		Data:      data,
	})
}

// writePage writes a page in the layout.
func (u *UI) writePage(writer http.ResponseWriter, request *http.Request, status int, name string, content page) {
	var body bytes.Buffer

	err := pageTemplates[name].ExecuteTemplate(&body, "layout", content)
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot render a web UI page", "page", name, "error", err)
		http.Error(writer, "The gateway can't show this page. Try again later.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write(body.Bytes())
}

type serversPage struct {
	Notice    string
	Servers   []serverRow
	Instances []gateway.InstanceEntry
	// Workspaces is true when the page shows workspace availability.
	Workspaces bool
	Admin      bool
	// Owners shows each instance owner as people read it, by raw principal.
	Owners map[string]principal.Display
}

// HasReason is true when any listed instance has a reason; the table has
// no Reason column otherwise.
func (p serversPage) HasReason() bool {
	return slices.ContainsFunc(p.Instances, func(instance gateway.InstanceEntry) bool { return instance.Reason != "" })
}

// HasReadyAt is true when any listed instance has a ready time.
func (p serversPage) HasReadyAt() bool {
	return slices.ContainsFunc(p.Instances, func(instance gateway.InstanceEntry) bool { return instance.ReadyAt != nil })
}

// InstanceColumns are the headings of the instances table: the Reason and
// Ready since columns show only when some instance has a value for them.
func (p serversPage) InstanceColumns() []string {
	columns := []string{"Server", "Scope", "Owner", "Status"}

	if p.HasReason() {
		columns = append(columns, "Reason")
	}

	if p.HasReadyAt() {
		columns = append(columns, "Ready since")
	}

	return append(columns, "Version", "")
}

// serverRow is one server of the catalog table.
type serverRow struct {
	gateway.ServerInfo
	Availability availability
	// Instances is the number of live instances of the server; admins see it.
	Instances int
}

// workspaceState is whether a list allows the server in one workspace.
type workspaceState struct {
	Workspace string
	Allowed   bool
}

// availability says whether the user's defaults and lists allow a server.
type availability struct {
	// VerifiedDefault is for verified workspaces without a list.
	VerifiedDefault bool
	// Lists are the verified workspaces that have a list.
	Lists      []workspaceState
	Unverified bool
}

// availabilityOf reads the server's availability from the user's defaults
// and lists.
func availabilityOf(scopes gateway.WorkspaceScopes, server string) availability {
	result := availability{VerifiedDefault: scopes.VerifiedDefault == gateway.ScopeAll}

	for _, list := range scopes.Lists {
		result.Lists = append(result.Lists, workspaceState{Workspace: list.Workspace, Allowed: slices.Contains(list.Servers, server)})
	}

	switch scopes.UnverifiedDefault {
	case gateway.ScopeAll:
		result.Unverified = true
	case gateway.ScopeList:
		result.Unverified = slices.Contains(scopes.UnverifiedServers, server)
	}

	return result
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

	infos, err := u.options.Servers.Catalog(ctx, sessionIdentity(session))
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

	data := serversPage{
		Notice:     serversNotices[request.URL.Query().Get("notice")],
		Instances:  instances,
		Workspaces: u.options.Workspaces != nil,
		Admin:      session.Admin,
	}

	owners := make([]string, 0, len(instances))
	for _, instance := range instances {
		owners = append(owners, instance.Owner)
	}

	names := u.namesFor(ctx, session.Principal, owners...)
	data.Owners = make(map[string]principal.Display, len(owners))

	for _, owner := range owners {
		data.Owners[owner] = names.show(owner)
	}

	var scopes gateway.WorkspaceScopes

	if data.Workspaces {
		scopes, err = u.options.Workspaces.Scopes(ctx, session.Principal)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot read workspace lists for the web UI", "error", err)
			http.Error(writer, "The gateway can't read your workspace lists. Try again later.", http.StatusInternalServerError)

			return
		}
	}

	for _, info := range infos {
		row := serverRow{ServerInfo: info, Availability: availabilityOf(scopes, info.Name)}

		for _, instance := range instances {
			if instance.Server == info.Name {
				row.Instances++
			}
		}

		data.Servers = append(data.Servers, row)
	}

	u.render(writer, request, "servers", "Servers", data)
}

type serverPage struct {
	Server     serverRow
	Tools      []gateway.ToolInfo
	Workspaces bool
}

// serverDetail shows one server and the tools the user may see in it. A
// server the user has no access to gives 404, the same as one that doesn't
// exist.
func (u *UI) serverDetail(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	info, tools, found, err := u.options.Servers.ServerDetail(ctx, sessionIdentity(session), request.PathValue("name"))
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read a server for the web UI", "error", err)
		http.Error(writer, "The gateway can't read the server. Try again later.", http.StatusInternalServerError)

		return
	}

	if !found {
		http.NotFound(writer, request)

		return
	}

	data := serverPage{Server: serverRow{ServerInfo: info}, Tools: tools, Workspaces: u.options.Workspaces != nil}

	if data.Workspaces {
		scopes, err := u.options.Workspaces.Scopes(ctx, session.Principal)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot read workspace lists for the web UI", "error", err)
			http.Error(writer, "The gateway can't read your workspace lists. Try again later.", http.StatusInternalServerError)

			return
		}

		data.Server.Availability = availabilityOf(scopes, info.Name)
	}

	u.render(writer, request, "server", info.Name, data)
}

// restartInstance requests the restart of an outdated instance.
// Another user's instance, or a
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

// pendingApprovals counts principal's pending approvals for the sidebar.
// The badge is a hint, so a failed read shows no badge rather than no page.
func (u *UI) pendingApprovals(ctx context.Context, principal string) int {
	if u.options.Database == nil || principal == "" {
		return 0
	}

	rows, err := u.queries.ListPendingApprovalsOfPrincipal(ctx, principal)
	if err != nil {
		u.logger.WarnContext(ctx, "cannot count the pending approvals for the web UI", "error", err)

		return 0
	}

	return len(rows)
}
