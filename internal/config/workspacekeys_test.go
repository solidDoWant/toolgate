package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func TestWorkspaceKeysSetting(t *testing.T) {
	const ui = "oidcUIClientID: toolgate-ui\npublicURL: https://toolgate.example.com\n"

	const attestation = "workspaceAttestation:\n  namespaces: [coder]\n  match: {by: email}\n  workspaceID: coder/{username}/{workspace}\n"

	tests := []struct {
		name        string
		yaml        string
		wantEnabled bool
		wantPrefix  string
		wantProblem string
	}{
		{name: "off by default", wantPrefix: "registered"},
		{name: "on with the default prefix", yaml: ui + "workspaceKeys: true\n", wantEnabled: true, wantPrefix: "registered"},
		{name: "custom prefix", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: keys.v1_x-y\n", wantEnabled: true, wantPrefix: "keys.v1_x-y"},
		{name: "prefix of 63 characters", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: " + strings63 + "\n", wantEnabled: true, wantPrefix: strings63},
		{name: "claimed prefix", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: claimed\n", wantProblem: "workspaceKeysPrefix"},
		{name: "k8s prefix", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: k8s\n", wantProblem: "workspaceKeysPrefix"},
		{name: "empty prefix", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: \"\"\n", wantProblem: "workspaceKeysPrefix"},
		{name: "prefix with a slash", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: a/b\n", wantProblem: "workspaceKeysPrefix"},
		{name: "prefix of 64 characters", yaml: ui + "workspaceKeys: true\nworkspaceKeysPrefix: " + strings63 + "a\n", wantProblem: "workspaceKeysPrefix"},
		{name: "prefix rules are skipped when off", yaml: "workspaceKeysPrefix: claimed\n", wantPrefix: "claimed"},
		{
			name:        "prefix equals the attestation workspace first segment",
			yaml:        ui + attestation + "workspaceKeys: true\nworkspaceKeysPrefix: coder\n",
			wantProblem: "workspaceKeysPrefix",
		},
		{
			name:        "without the web UI",
			yaml:        "workspaceKeys: true\n",
			wantProblem: "workspaceKeys: needs the web UI (oidcUIClientID), because only the web UI can approve a key.",
		},
		{name: "both on", yaml: ui + attestation + "workspaceKeys: true\n", wantEnabled: true, wantPrefix: "registered"},
		{name: "attestation only", yaml: attestation, wantPrefix: "registered"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n"+test.yaml)

			cfg, err := config.Load(path, envFrom(nil))
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.wantEnabled, cfg.WorkspaceKeys)
			assert.Equal(t, test.wantPrefix, cfg.WorkspaceKeysPrefix)
		})
	}

	t.Run("environment variables", func(t *testing.T) {
		path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n"+ui)

		cfg, err := config.Load(path, envFrom(map[string]string{
			"TOOLGATE_WORKSPACE_KEYS":        "true",
			"TOOLGATE_WORKSPACE_KEYS_PREFIX": "mine",
		}))
		require.NoError(t, err)
		assert.True(t, cfg.WorkspaceKeys)
		assert.Equal(t, "mine", cfg.WorkspaceKeysPrefix)
	})
}

const strings63 = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk"
