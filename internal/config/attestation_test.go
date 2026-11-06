package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func TestWorkspaceAttestationSetting(t *testing.T) {
	const spiffe = "spiffeTrustDomains:\n  - {name: coder.example.com, jwksURL: \"https://oidc.example/keys\"}\n"

	tests := []struct {
		name        string
		yaml        string
		wantProblem string
	}{
		{
			name: "email match",
			yaml: "workspaceAttestation:\n  namespaces: [coder]\n  match: {by: email}\n  workspaceID: coder/{username}/{workspace}\n",
		},
		{
			name: "SPIFFE pattern",
			yaml: spiffe + "workspaceAttestation:\n  namespaces: [coder]\n  match: {by: claim, claim: preferred_username, key: username}\n" +
				"  workspaceID: coder/{username}/{workspace}\n  spiffeIDPattern: spiffe://coder.example.com/coder/{username}/{workspace}\n",
		},
		{
			name:        "no namespaces",
			yaml:        "workspaceAttestation:\n  match: {by: email}\n  workspaceID: coder/{workspace}\n",
			wantProblem: "workspaceAttestation.namespaces: required",
		},
		{
			name:        "claimed workspace",
			yaml:        "workspaceAttestation:\n  namespaces: [coder]\n  match: {by: email}\n  workspaceID: claimed/{workspace}\n",
			wantProblem: "workspaceAttestation.workspaceID",
		},
		{
			name: "pattern trust domain not configured",
			yaml: "workspaceAttestation:\n  namespaces: [coder]\n  match: {by: claim, claim: preferred_username, key: username}\n" +
				"  workspaceID: coder/{username}/{workspace}\n  spiffeIDPattern: spiffe://coder.example.com/coder/{username}/{workspace}\n",
			wantProblem: "workspaceAttestation.spiffeIDPattern",
		},
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
			require.NotNil(t, cfg.AttestationOptions())
			assert.Equal(t, []string{"coder"}, cfg.AttestationOptions().Namespaces)
		})
	}

	t.Run("absent is off", func(t *testing.T) {
		path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n")

		cfg, err := config.Load(path, envFrom(nil))
		require.NoError(t, err)
		assert.Nil(t, cfg.AttestationOptions())
	})
}
