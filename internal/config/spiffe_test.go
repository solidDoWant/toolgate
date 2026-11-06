package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func TestSPIFFETrustDomainsSetting(t *testing.T) {
	tests := []struct {
		name        string
		domains     string
		wantProblem string
	}{
		{
			name:    "bundle endpoint with a CA file",
			domains: "  - {name: coder.example.com, bundleEndpointURL: \"https://spire.example:8443\", caFile: /ca.pem}\n",
		},
		{
			name:    "JWKS",
			domains: "  - {name: coder.example.com, jwksURL: \"https://oidc.example/keys\"}\n",
		},
		{
			name:        "both URLs",
			domains:     "  - {name: coder.example.com, bundleEndpointURL: \"https://a.example\", jwksURL: \"https://b.example\"}\n",
			wantProblem: "spiffeTrustDomains[0]: set exactly one of bundleEndpointURL and jwksURL",
		},
		{
			name:        "no URL",
			domains:     "  - {name: coder.example.com}\n",
			wantProblem: "spiffeTrustDomains[0]: set exactly one of bundleEndpointURL and jwksURL",
		},
		{
			name:        "http bundle endpoint",
			domains:     "  - {name: coder.example.com, bundleEndpointURL: \"http://spire.example\"}\n",
			wantProblem: "spiffeTrustDomains[0].bundleEndpointURL",
		},
		{
			name:        "http JWKS",
			domains:     "  - {name: coder.example.com, jwksURL: \"http://oidc.example/keys\"}\n",
			wantProblem: "spiffeTrustDomains[0].jwksURL",
		},
		{
			name:        "invalid trust domain",
			domains:     "  - {name: \"Coder Example\", jwksURL: \"https://oidc.example/keys\"}\n",
			wantProblem: "spiffeTrustDomains[0].name",
		},
		{
			name:        "duplicate trust domain",
			domains:     "  - {name: coder.example.com, jwksURL: \"https://a.example\"}\n  - {name: coder.example.com, jwksURL: \"https://b.example\"}\n",
			wantProblem: "spiffeTrustDomains[1].name: \"coder.example.com\" is listed twice",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\nspiffeTrustDomains:\n"+test.domains)

			cfg, err := config.Load(path, envFrom(nil))
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)
			require.Len(t, cfg.SPIFFETrustDomains, 1)
			assert.Equal(t, "coder.example.com", cfg.SPIFFETrustDomains[0].Name)
		})
	}
}
