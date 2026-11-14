//go:build !windows

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/login"
)

func approvedGateway(body string) *keysGateway {
	return &keysGateway{postCode: 202, postBody: body, statuses: []string{"approved"}}
}

func TestWorkspaceRegisterUsesApprovedKey(t *testing.T) {
	t.Run("saves the key when no source is configured, and headers then sign", func(t *testing.T) {
		dir := keySourceEnvironment(t)
		run := runKeys(t, approvedGateway(pendingBody), dir, "x", "register", "dev")
		require.Equal(t, 0, run.code, run.stderr)

		path := filepath.Join(dir, "workspace-keys", "dev.pem")
		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)
		assert.Equal(t, path, settings.AttestationKeyFile)
		assert.Contains(t, run.stdout, "The key is now in use")
		assert.Contains(t, run.stdout, "Claude Code uses it when it next connects: run /mcp to reconnect a running session, or start a new one.")

		code, stdout, stderr := runCLI(t, "", "headers")
		require.Equal(t, 0, code, stderr)
		assert.NotEmpty(t, headerValues(t, stdout)["X-Toolgate-Attestation"])
	})

	t.Run("keeps another source and prints the switch command", func(t *testing.T) {
		dir := keySourceEnvironment(t)
		require.NoError(t, login.SaveSettings(dir, login.Settings{AttestationTokenFile: "/run/token"}))

		run := runKeys(t, approvedGateway(pendingBody), dir, "x", "register", "dev")
		require.Equal(t, 0, run.code, run.stderr)

		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)
		assert.Equal(t, "/run/token", settings.AttestationTokenFile)
		assert.Empty(t, settings.AttestationKeyFile)
		assert.Contains(t, run.stdout, "The token file /run/token stays in use.")
		assert.Contains(t, run.stdout, "toolgate configure --attestation-key-file "+filepath.Join(dir, "workspace-keys", "dev.pem"))
	})

	t.Run("--use replaces another source", func(t *testing.T) {
		dir := keySourceEnvironment(t)
		require.NoError(t, login.SaveSettings(dir, login.Settings{AttestationTokenFile: "/run/token"}))

		run := runKeys(t, approvedGateway(pendingBody), dir, "x", "register", "dev", "--use")
		require.Equal(t, 0, run.code, run.stderr)

		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)
		assert.Empty(t, settings.AttestationTokenFile)
		assert.Equal(t, filepath.Join(dir, "workspace-keys", "dev.pem"), settings.AttestationKeyFile)
	})

	t.Run("--no-use leaves config.json alone", func(t *testing.T) {
		dir := keySourceEnvironment(t)
		run := runKeys(t, approvedGateway(pendingBody), dir, "x", "register", "dev", "--no-use")
		require.Equal(t, 0, run.code, run.stderr)

		settings, err := login.LoadSettings(dir)
		if err == nil {
			assert.Empty(t, settings.AttestationKeyFile)
		}

		assert.Contains(t, run.stdout, "toolgate configure --attestation-key-file")
	})

	t.Run("--use and --no-use together are refused", func(t *testing.T) {
		run := runKeys(t, approvedGateway(pendingBody), t.TempDir(), "x", "register", "dev", "--use", "--no-use")
		assert.Equal(t, 2, run.code)
	})

	t.Run("stopping before approval changes nothing and prints the command", func(t *testing.T) {
		for _, status := range []string{"expired", "pending"} {
			dir := keySourceEnvironment(t)
			gateway := &keysGateway{postCode: 202, postBody: pendingBody, statuses: []string{status}}

			if status == "pending" {
				gateway.statuses = []string{"pending", "expired"}
			}

			run := runKeys(t, gateway, dir, "x", "register", "dev")
			assert.Equal(t, 1, run.code)

			settings, err := login.LoadSettings(dir)
			if err == nil {
				assert.Empty(t, settings.AttestationKeyFile)
			}

			assert.Contains(t, run.stdout, "config.json is unchanged. After the request is approved, run: toolgate configure --attestation-key-file")
		}
	})

	t.Run("switches when the replaced key was the configured source", func(t *testing.T) {
		dir := keySourceEnvironment(t)
		_, oldPath := newWorkspaceKey(t)
		require.NoError(t, login.SaveSettings(dir, login.Settings{AttestationKeyFile: oldPath}))

		_, oldFingerprint, err := checkKeyFile(oldPath)
		require.NoError(t, err)

		body := `{"request":"abc","url":"https://gw/ui/approvals/abc","name":"dev","workspace":"registered/alice/dev","fingerprint":"SHA256:xyz","replaces":"` + oldFingerprint + `"}`
		run := runKeys(t, approvedGateway(body), dir, "x", "register", "dev")
		require.Equal(t, 0, run.code, run.stderr)

		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "workspace-keys", "dev.pem"), settings.AttestationKeyFile)
	})

	t.Run("keeps a configured key that is not the replaced one", func(t *testing.T) {
		dir := keySourceEnvironment(t)
		_, otherPath := newWorkspaceKey(t)
		require.NoError(t, login.SaveSettings(dir, login.Settings{AttestationKeyFile: otherPath}))

		body := `{"request":"abc","url":"https://gw/ui/approvals/abc","name":"dev","workspace":"registered/alice/dev","fingerprint":"SHA256:xyz","replaces":"SHA256:old"}`
		run := runKeys(t, approvedGateway(body), dir, "x", "register", "dev")
		require.Equal(t, 0, run.code, run.stderr)

		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)
		assert.Equal(t, otherPath, settings.AttestationKeyFile)
	})
}
