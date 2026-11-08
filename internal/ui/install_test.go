package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sampleGateway = "https://toolgate.example.com"
	sampleVersion = "0.4.7"
)

func TestRenderInstallScriptRefusesValuesThatCouldInject(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		version string
		wantErr bool
	}{
		{name: "plain values", url: sampleGateway, version: sampleVersion},
		{name: "trailing slash is dropped", url: sampleGateway + "/", version: "0.4.7-rc.1"},
		{name: "dev build", url: sampleGateway, version: "dev"},
		{name: "http URL", url: "http://toolgate.example.com", version: sampleVersion, wantErr: true},
		{name: "quote in URL", url: "https://x.example/'; rm -rf ~ #", version: sampleVersion, wantErr: true},
		{name: "command substitution in URL", url: "https://x.example/$(id)", version: sampleVersion, wantErr: true},
		{name: "newline in URL", url: "https://x.example/\nid", version: sampleVersion, wantErr: true},
		{name: "JSON quote in URL", url: `https://x.example/"}`, version: sampleVersion, wantErr: true},
		{name: "injection in version", url: sampleGateway, version: "1.0.0'; id; '", wantErr: true},
		{name: "empty version", url: sampleGateway, version: "", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script, err := renderInstallScript(test.url, test.version)
			if test.wantErr {
				require.Error(t, err)
				assert.Empty(t, script)

				return
			}

			require.NoError(t, err)
			assert.Contains(t, string(script), "GATEWAY_URL='"+strings.TrimSuffix(test.url, "/")+"'\n")
			assert.Contains(t, string(script), "VERSION='"+test.version+"'\n")
		})
	}
}

func TestShellQuoteKeepsAnyValueOneWord(t *testing.T) {
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
	assert.Equal(t, `'$(id) `+"`id`"+`'`, shellQuote("$(id) `id`"))
}

func TestInstallScriptPassesShellcheck(t *testing.T) {
	shellcheck, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck is not installed")
	}

	script, err := renderInstallScript(sampleGateway, sampleVersion)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "install.sh")
	require.NoError(t, os.WriteFile(path, script, 0o600))

	output, err := exec.CommandContext(t.Context(), shellcheck, "--shell=sh", "--severity=style", path).CombinedOutput()
	require.NoError(t, err, string(output))
}

// installEnv is a PATH holding only stubs and the basic tools the installer
// needs, so a real claude or toolgate on the machine never runs.
type installEnv struct {
	dir     string
	bin     string
	log     string
	script  string
	install string
}

func newInstallEnv(t *testing.T, withClaude bool) installEnv {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Skip("the installer test needs Linux")
	}

	dir := t.TempDir()
	env := installEnv{dir: dir, bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "log"), install: filepath.Join(dir, "install")}
	require.NoError(t, os.MkdirAll(env.bin, 0o755))

	for _, tool := range []string{"uname", "mktemp", "rm", "awk", "cut", "sha256sum", "mkdir", "chmod", "mv", "cat", "install"} {
		path, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(env.bin, tool)))
	}

	logging := func(name, extra string) {
		body := "#!/bin/sh\necho \"" + name + " $*\" >> " + env.log + "\n" + extra
		require.NoError(t, os.WriteFile(filepath.Join(env.bin, name), []byte(body), 0o755))
	}

	// The curl stub serves a fake binary and a matching checksums file.
	logging("curl", `out=; while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
case $out in
*checksums.txt) printf '%s  toolgate-linux-amd64\n%s  toolgate-linux-arm64\n' "$FAKE_SUM" "$FAKE_SUM" > "$out" ;;
*) printf '#!/bin/sh\necho "toolgate $*" >> `+env.log+`\n' > "$out" ;;
esac
`)

	if withClaude {
		logging("claude", "cat >> "+env.log+" 2>/dev/null || true\n")
	}

	script, err := renderInstallScript(sampleGateway, sampleVersion)
	require.NoError(t, err)

	env.script = filepath.Join(dir, "install.sh")
	require.NoError(t, os.WriteFile(env.script, script, 0o600))

	return env
}

func (e installEnv) run(t *testing.T, sum string, args ...string) (string, error) {
	t.Helper()

	sh, err := exec.LookPath("sh")
	require.NoError(t, err)

	// A command without a controlling terminal cannot prompt.
	command := exec.CommandContext(t.Context(), sh, append([]string{e.script}, args...)...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Env = []string{"PATH=" + e.bin, "HOME=" + e.dir, "TOOLGATE_INSTALL_DIR=" + e.install, "FAKE_SUM=" + sum}
	output, err := command.CombinedOutput()

	return string(output), err
}

func (e installEnv) logged(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(e.log)
	if os.IsNotExist(err) {
		return ""
	}

	require.NoError(t, err)

	return string(data)
}

func TestInstallScriptDryRunPrintsStepsAndPinsTheVersion(t *testing.T) {
	env := newInstallEnv(t, true)

	output, err := env.run(t, "", "--dry-run")
	require.NoError(t, err, output)

	base := "https://github.com/solidDoWant/toolgate/releases/download/" + sampleVersion

	assert.Contains(t, output, "curl -fsSL -o")
	assert.Contains(t, output, base+"/toolgate-linux-")
	assert.Contains(t, output, base+"/checksums.txt")
	assert.Contains(t, output, "claude plugin marketplace add solidDoWant/toolgate")
	assert.Contains(t, output, "claude plugin install toolgate@toolgate")
	assert.Contains(t, output, "claude plugin configure toolgate@toolgate --values-stdin")
	assert.Contains(t, output, "configure --gateway "+sampleGateway)
	assert.Contains(t, output, "offer to run: toolgate login")
	assert.NotContains(t, output, "/latest/")

	assert.Empty(t, env.logged(t), "a dry run runs no command")
	assert.NoDirExists(t, env.install)
}

func TestInstallScriptDryRunWithoutClaudePrintsTheCommands(t *testing.T) {
	env := newInstallEnv(t, false)

	output, err := env.run(t, "", "--dry-run")
	require.NoError(t, err, output)

	assert.Contains(t, output, "claude was not found")
	assert.Contains(t, output, "claude plugin install toolgate@toolgate")
	assert.NotContains(t, output, "[dry run] claude")
}

func TestInstallScriptInstallsAVerifiedBinaryAndConfiguresIt(t *testing.T) {
	env := newInstallEnv(t, true)

	// The curl stub writes this exact content as the binary.
	content := fmt.Sprintf("#!/bin/sh\necho \"toolgate $*\" >> %s\n", env.log)
	sum := sha256.Sum256([]byte(content))

	output, err := env.run(t, hex.EncodeToString(sum[:]))
	require.NoError(t, err, output)

	assert.Contains(t, output, "Checksum verified")
	assert.FileExists(t, filepath.Join(env.install, "toolgate"))
	assert.Contains(t, output, "To sign in, run: toolgate login")

	logged := env.logged(t)
	assert.Contains(t, logged, "claude plugin marketplace add solidDoWant/toolgate")
	assert.Contains(t, logged, "claude plugin install toolgate@toolgate")
	assert.Contains(t, logged, "claude plugin configure toolgate@toolgate --values-stdin")
	assert.Contains(t, logged, `{"gateway_url":"`+sampleGateway+`"}`)
	assert.Contains(t, logged, "toolgate configure --gateway "+sampleGateway)

	// Running it again updates in place.
	_, err = env.run(t, hex.EncodeToString(sum[:]))
	require.NoError(t, err)
}

func TestInstallScriptAbortsOnChecksumMismatch(t *testing.T) {
	env := newInstallEnv(t, true)

	output, err := env.run(t, strings.Repeat("0", 64))
	require.Error(t, err)
	assert.Contains(t, output, "checksum mismatch")
	assert.NoFileExists(t, filepath.Join(env.install, "toolgate"))
	assert.NotContains(t, env.logged(t), "claude")
}

func TestInstallEndpointServesTheScriptWithoutASession(t *testing.T) {
	webUI, registrar := newTestUI(t)
	_ = webUI

	recorder := httptest.NewRecorder()
	registrar.mux.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, InstallPath, nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "text/x-shellscript")
	assert.True(t, strings.HasPrefix(recorder.Body.String(), "#!/bin/sh\n"))
	assert.Contains(t, recorder.Body.String(), "GATEWAY_URL='https://toolgate.example'")
}

func TestSetupPageShowsTheGatewayURLAndLoadsCopyScriptFromTheGateway(t *testing.T) {
	webUI, _ := newTestUI(t)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, SetupPath, nil)
	webUI.setup(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	body := recorder.Body.String()
	assert.Contains(t, body, "curl -fsSL https://toolgate.example/install.sh | sh")
	assert.Contains(t, body, `<script src="/ui/static/copy.js" defer></script>`)
	assert.NotContains(t, body, "<script>")
	assert.NotContains(t, body, "For admins")
	assert.Contains(t, body, `<script src="/ui/static/ui.js" defer></script>`)
	assert.Contains(t, body, `<h1 class="m-0 text-2xl font-semibold">Quick setup</h1>`)
	assert.Equal(t, 5, strings.Count(body, "<section "), "one card for each section")
	assert.Equal(t, 3, strings.Count(body, "<script"), "only htmx, the copy script and the components script, each with a src")
}

func newTestUI(t *testing.T) (*UI, nopRegistrar) {
	t.Helper()

	registrar := nopRegistrar{http.NewServeMux()}
	webUI := New(Options{IssuerURL: "https://idp.invalid", ClientID: "toolgate-ui", PublicURL: "https://toolgate.example"}, registrar)

	return webUI, registrar
}
