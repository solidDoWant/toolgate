package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func audienceList(count int) string {
	items := make([]string, 0, count)
	for index := range count {
		items = append(items, "client-"+string(rune('a'+index)))
	}

	return "[" + strings.Join(items, ", ") + "]"
}

func TestOIDCAudienceSetting(t *testing.T) {
	tests := []struct {
		name          string
		file          string
		env           string
		wantAudiences config.Audiences
		wantProblem   string
	}{
		{
			name:          "one string in YAML",
			file:          "oidcAudience: toolgate\n",
			wantAudiences: config.Audiences{"toolgate"},
		},
		{
			name:          "a list in YAML",
			file:          "oidcAudience: [toolgate, coder-toolgate]\n",
			wantAudiences: config.Audiences{"toolgate", "coder-toolgate"},
		},
		{
			name:          "one value in the environment",
			env:           "toolgate",
			wantAudiences: config.Audiences{"toolgate"},
		},
		{
			name:          "comma-separated values in the environment",
			env:           "toolgate, coder-toolgate",
			wantAudiences: config.Audiences{"toolgate", "coder-toolgate"},
		},
		{
			name:          "the environment replaces the file",
			file:          "oidcAudience: [a, b]\n",
			env:           "toolgate",
			wantAudiences: config.Audiences{"toolgate"},
		},
		{
			name:          "8 audiences",
			file:          "oidcAudience: " + audienceList(8) + "\n",
			wantAudiences: config.Audiences{"client-a", "client-b", "client-c", "client-d", "client-e", "client-f", "client-g", "client-h"},
		},
		{
			name:        "9 audiences",
			file:        "oidcAudience: " + audienceList(9) + "\n",
			wantProblem: "oidcAudience: 9 audiences, at most 8 are allowed",
		},
		{
			name:        "an empty list",
			file:        "oidcAudience: []\n",
			wantProblem: "oidcAudience: required",
		},
		{
			name:        "an empty string",
			file:        "oidcAudience: \"\"\n",
			wantProblem: "oidcAudience: an audience must not be empty",
		},
		{
			name:        "an empty item",
			file:        "oidcAudience: [toolgate, \"\"]\n",
			wantProblem: "oidcAudience: an audience must not be empty",
		},
		{
			name:        "a duplicate",
			file:        "oidcAudience: [toolgate, coder, toolgate]\n",
			wantProblem: `oidcAudience: "toolgate" is listed more than once`,
		},
		{
			name:          "256 characters",
			file:          "oidcAudience: " + strings.Repeat("a", 256) + "\n",
			wantAudiences: config.Audiences{strings.Repeat("a", 256)},
		},
		{
			name:        "257 characters",
			file:        "oidcAudience: " + strings.Repeat("a", 257) + "\n",
			wantProblem: "oidcAudience: an audience is longer than 256 characters",
		},
		{
			name:        "not a string or a list",
			file:        "oidcAudience: {name: toolgate}\n",
			wantProblem: "must be a string or a list of strings",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\n"+test.file)

			cfg, err := config.Load(path, envFrom(map[string]string{"TOOLGATE_OIDC_AUDIENCE": test.env}))
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.wantAudiences, cfg.OIDCAudience)
		})
	}
}
