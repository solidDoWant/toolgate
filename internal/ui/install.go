package ui

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"text/template"

	"github.com/solidDoWant/toolgate/internal/version"
)

// InstallPath serves the shell installer without authentication. It holds no
// secret: only the gateway's public URL and version.
const InstallPath = "/install.sh"

// SetupPath is the Quick setup page.
const SetupPath = "/ui/setup"

var (
	installURLPattern     = regexp.MustCompile(`^https://[A-Za-z0-9._~:/%+-]+$`)
	installVersionPattern = regexp.MustCompile(`^(dev|[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?)$`)

	installTemplate = template.Must(template.New("install.sh.tmpl").Funcs(template.FuncMap{"shq": shellQuote}).
			ParseFS(templateFiles, "templates/install.sh.tmpl"))
)

// shellQuote quotes value as one POSIX shell word.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// renderInstallScript fills the installer with the gateway URL and version.
// It refuses values that are not a plain https URL and a release version, so
// configuration can never inject shell or JSON into the script.
func renderInstallScript(gatewayURL, releaseVersion string) ([]byte, error) {
	gatewayURL = strings.TrimSuffix(gatewayURL, "/")

	if !installURLPattern.MatchString(gatewayURL) {
		return nil, errors.New("the gateway URL is not a plain https URL")
	}

	if !installVersionPattern.MatchString(releaseVersion) {
		return nil, fmt.Errorf("the version %q is not a release version", releaseVersion)
	}

	var script bytes.Buffer

	err := installTemplate.ExecuteTemplate(&script, "install.sh.tmpl", struct{ GatewayURL, Version string }{gatewayURL, releaseVersion})
	if err != nil {
		return nil, fmt.Errorf("render the installer: %w", err)
	}

	return script.Bytes(), nil
}

// installScript serves the installer.
func (u *UI) installScript(writer http.ResponseWriter, request *http.Request) {
	script, err := renderInstallScript(u.options.PublicURL, version.Get())
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot render the installer", "error", err)
		http.Error(writer, "The gateway can't serve the installer. Try again later.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = writer.Write(script)
}

type setupPage struct {
	GatewayURL string
	Version    string
	Admin      bool
}

// setup shows the Quick setup page with this gateway's URL in every command.
func (u *UI) setup(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())

	u.render(writer, request, "setup", "Quick setup", setupPage{
		GatewayURL: strings.TrimSuffix(u.options.PublicURL, "/"),
		Version:    version.Get(),
		Admin:      session.Admin,
	})
}
