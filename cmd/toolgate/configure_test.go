//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// pluginHelperEnvironment sets up the environment Claude Code gives a
// plugin's headersHelper: the variables with credential-like names are
// removed, the session and workspace are kept.
func pluginHelperEnvironment(t *testing.T) string {
	t.Helper()

	dir := isolateUserConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "dev")

	for _, name := range []string{"TOOLGATE_TOKEN_COMMAND", "TOOLGATE_ATTESTATION_TOKEN_FILE", "TOOLGATE_ATTESTATION_TOKEN_REQUEST", "TOOLGATE_ATTESTATION_SPIFFE_SOCKET", "TOOLGATE_ATTESTATION_SIGNER_FILE", "TOOLGATE_SPIFFE_SOCKET"} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}

	return dir
}

func workloadTokenFile(t *testing.T, content string) string {
	t.Helper()

	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))

	return file
}

func headerValues(t *testing.T, stdout string) map[string]string {
	t.Helper()

	var values map[string]string
	require.NoError(t, json.Unmarshal([]byte(stdout), &values))

	return values
}

func TestHeadersUseTheConfiguredTokenCommandAndAttestationWithoutTheVariables(t *testing.T) {
	dir := pluginHelperEnvironment(t)
	file := workloadTokenFile(t, "workload-one\n")

	code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token", "--attestation-token-file", file, "--gateway", "https://gateway.example.com/")
	require.Equal(t, 0, code, stderr)

	info, err := os.Stat(filepath.Join(dir, login.SettingsFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	code, stdout, stderr := runCLI(t, "", "headers")
	require.Equal(t, 0, code, stderr)

	values := headerValues(t, stdout)
	assert.Equal(t, "Bearer user-token", values["Authorization"])
	assert.Equal(t, "workload-one", values["X-Toolgate-Attestation"])
	assert.Equal(t, "session-1", values["X-Toolgate-Session"])

	settings, err := login.LoadSettings(dir)
	require.NoError(t, err)
	assert.Equal(t, "https://gateway.example.com", settings.Gateway)
}

func TestHeadersPreferTheEnvironmentOverTheConfigFile(t *testing.T) {
	pluginHelperEnvironment(t)
	fileToken := workloadTokenFile(t, "from-config")
	envToken := workloadTokenFile(t, "from-environment")

	code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo config-token", "--attestation-token-file", fileToken)
	require.Equal(t, 0, code, stderr)

	t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo environment-token")
	t.Setenv("TOOLGATE_ATTESTATION_TOKEN_FILE", envToken)

	code, stdout, stderr := runCLI(t, "", "headers")
	require.Equal(t, 0, code, stderr)

	values := headerValues(t, stdout)
	assert.Equal(t, "Bearer environment-token", values["Authorization"])
	assert.Equal(t, "from-environment", values["X-Toolgate-Attestation"])
}

func TestConfigureKeepsTheSettingsItIsNotGiven(t *testing.T) {
	dir := pluginHelperEnvironment(t)

	code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token", "--attestation-token-file", "/run/token")
	require.Equal(t, 0, code, stderr)

	code, _, stderr = runCLI(t, "", "configure", "--gateway", "https://gateway.example.com")
	require.Equal(t, 0, code, stderr)

	settings, err := login.LoadSettings(dir)
	require.NoError(t, err)
	assert.Equal(t, "echo user-token", settings.TokenCommand)
	assert.Equal(t, "/run/token", settings.AttestationTokenFile)
	assert.Equal(t, "https://gateway.example.com", settings.Gateway)

	code, _, stderr = runCLI(t, "", "configure", "--attestation-token-file", "")
	require.Equal(t, 0, code, stderr)

	settings, err = login.LoadSettings(dir)
	require.NoError(t, err)
	assert.Empty(t, settings.AttestationTokenFile, "an empty value removes the setting")
	assert.Equal(t, "echo user-token", settings.TokenCommand)
}

func TestConfigureRefusesInvalidSettings(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "no settings", args: nil},
		{name: "gateway that is not an http URL", args: []string{"--gateway", "ftp://gateway"}},
		{name: "two attestation sources", args: []string{"--attestation-token-file", "/a", "--attestation-token-request", "/b"}},
		{name: "unexpected argument", args: []string{"--gateway", "https://gateway", "extra"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := pluginHelperEnvironment(t)

			code, _, _ := runCLI(t, "", append([]string{"configure"}, test.args...)...)
			assert.Equal(t, 2, code)
			assert.NoFileExists(t, filepath.Join(dir, login.SettingsFileName))
		})
	}
}

func TestHeadersWaitForASessionStartHookThatRunsLate(t *testing.T) {
	pluginHelperEnvironment(t)
	t.Setenv("TOOLGATE_SESSION", "")
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getppid()))

	code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token")
	require.Equal(t, 0, code, stderr)

	// Claude Code starts the helper and the hook at the same time; here the
	// hook starts 2 s after the helper.
	hookDone := make(chan int, 1)

	go func() {
		time.Sleep(2 * time.Second)

		code, _, _ := runCLI(t, `{"session_id":"late-session"}`, "hook", "session-start")
		hookDone <- code
	}()

	code, stdout, stderr := runCLI(t, "", "headers")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, 0, <-hookDone)

	values := headerValues(t, stdout)
	assert.Equal(t, "late-session", values["X-Toolgate-Session"])
	assert.Equal(t, "Bearer user-token", values["Authorization"])
}

func TestLoginKeepsTheConfiguredSettings(t *testing.T) {
	dir := isolateUserConfig(t)

	code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token", "--attestation-token-request", "/run/attestation")
	require.Equal(t, 0, code, stderr)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr = signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, stderr)

	settings, err := login.LoadSettings(dir)
	require.NoError(t, err)
	assert.Equal(t, "alice", settings.Subject)
	assert.Equal(t, "echo user-token", settings.TokenCommand)
	assert.Equal(t, "/run/attestation", settings.AttestationTokenRequest)
}
