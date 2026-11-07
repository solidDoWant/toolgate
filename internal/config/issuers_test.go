package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/config"
)

func issuerList(count int) string {
	var builder strings.Builder

	for index := range count {
		fmt.Fprintf(&builder, "  - {issuerURL: https://idp.example/o/client-%d/, audience: client-%d}\n", index, index)
	}

	return builder.String()
}

func TestOIDCAdditionalIssuersSetting(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		want        []auth.IssuerOptions
		wantProblem string
	}{
		{
			name: "absent",
			file: "",
			want: []auth.IssuerOptions{},
		},
		{
			name: "one issuer with one audience",
			file: "oidcAdditionalIssuers:\n  - issuerURL: https://idp.example/o/coder-toolgate/\n    audience: coder-toolgate\n",
			want: []auth.IssuerOptions{{IssuerURL: "https://idp.example/o/coder-toolgate/", Audiences: []string{"coder-toolgate"}}},
		},
		{
			name: "an issuer with a list of audiences",
			file: "oidcAdditionalIssuers:\n  - issuerURL: http://idp.example/coder\n    audience: [coder, coder-2]\n",
			want: []auth.IssuerOptions{{IssuerURL: "http://idp.example/coder", Audiences: []string{"coder", "coder-2"}}},
		},
		{
			name: "8 issuers",
			file: "oidcAdditionalIssuers:\n" + issuerList(8),
		},
		{
			name:        "9 issuers",
			file:        "oidcAdditionalIssuers:\n" + issuerList(9),
			wantProblem: "oidcAdditionalIssuers: 9 issuers, at most 8 are allowed",
		},
		{
			name:        "no issuer URL",
			file:        "oidcAdditionalIssuers:\n  - audience: coder\n",
			wantProblem: "oidcAdditionalIssuers[0].issuerURL: required",
		},
		{
			name:        "not an http URL",
			file:        "oidcAdditionalIssuers:\n  - issuerURL: ftp://idp.example\n    audience: coder\n",
			wantProblem: `oidcAdditionalIssuers[0].issuerURL: "ftp://idp.example" is not an http or https URL`,
		},
		{
			name:        "the primary issuer",
			file:        "oidcAdditionalIssuers:\n  - issuerURL: https://idp.example\n    audience: coder\n",
			wantProblem: `oidcAdditionalIssuers[0].issuerURL: "https://idp.example" is already configured`,
		},
		{
			name:        "an issuer listed twice",
			file:        "oidcAdditionalIssuers:\n  - {issuerURL: https://idp.example/coder, audience: a}\n  - {issuerURL: https://idp.example/coder, audience: b}\n",
			wantProblem: `oidcAdditionalIssuers[1].issuerURL: "https://idp.example/coder" is already configured`,
		},
		{
			name:        "no audience",
			file:        "oidcAdditionalIssuers:\n  - issuerURL: https://idp.example/coder\n",
			wantProblem: "oidcAdditionalIssuers[0].audience: required",
		},
		{
			name:        "9 audiences",
			file:        "oidcAdditionalIssuers:\n  - issuerURL: https://idp.example/coder\n    audience: " + audienceList(9) + "\n",
			wantProblem: "oidcAdditionalIssuers[0].audience: 9 audiences, at most 8 are allowed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n"+test.file)

			cfg, err := config.Load(path, envFrom(nil))
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)

			if test.want != nil {
				assert.Equal(t, test.want, cfg.AdditionalIssuers())
			}
		})
	}
}
