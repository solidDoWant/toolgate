package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func TestTokenExchangesSettingIsRefused(t *testing.T) {
	content := "databaseURL: " + validDatabaseURL + "\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\nsecretsKeysetFile: /keys/keyset.json\n" +
		"tokenExchanges:\n  - {name: temporal, tokenURL: \"https://idp.example/token\", clientID: c, clientSecretFile: /s}\n"

	_, err := config.Load(writeFile(t, content), envFrom(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenExchanges")
}

func TestTokenExchangeCAFileSetting(t *testing.T) {
	content := "databaseURL: " + validDatabaseURL + "\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\ntokenExchangeCAFile: /etc/toolgate/token-exchange/ca.pem\n"

	cfg, err := config.Load(writeFile(t, content), envFrom(nil))
	require.NoError(t, err)
	assert.Equal(t, "/etc/toolgate/token-exchange/ca.pem", cfg.TokenExchangeCAFile)
}

func TestTokenExchangeAllowedTokenURLsSetting(t *testing.T) {
	base := "databaseURL: " + validDatabaseURL + "\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n"

	urls := func(count int) string {
		var builder strings.Builder

		builder.WriteString("tokenExchangeAllowedTokenURLs:\n")

		for index := range count {
			builder.WriteString("  - https://idp.example/token/" + strings.Repeat("a", index+1) + "\n")
		}

		return builder.String()
	}

	tests := []struct {
		name        string
		content     string
		want        []string
		wantProblem string
	}{
		{name: "unset", content: "", want: nil},
		{name: "one URL", content: "tokenExchangeAllowedTokenURLs: [\"https://idp.example/token\"]\n", want: []string{"https://idp.example/token"}},
		{name: "sixteen URLs", content: urls(16)},
		{name: "seventeen URLs", content: urls(17), wantProblem: "tokenExchangeAllowedTokenURLs: at most 16 items, got 17"},
		{name: "http URL", content: "tokenExchangeAllowedTokenURLs: [\"http://idp.example/token\"]\n", wantProblem: "tokenExchangeAllowedTokenURLs[0]: must be an https:// URL"},
		{name: "URL without host", content: "tokenExchangeAllowedTokenURLs: [\"https://\"]\n", wantProblem: "tokenExchangeAllowedTokenURLs[0]: must be an https:// URL"},
		{name: "URL of 2049 characters", content: "tokenExchangeAllowedTokenURLs: [\"https://idp.example/" + strings.Repeat("a", 2049-len("https://idp.example/")) + "\"]\n", wantProblem: "tokenExchangeAllowedTokenURLs[0]: must be an https:// URL of at most 2048 characters"},
		{name: "URL of 2048 characters", content: "tokenExchangeAllowedTokenURLs: [\"https://idp.example/" + strings.Repeat("a", 2048-len("https://idp.example/")) + "\"]\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.Load(writeFile(t, base+test.content), envFrom(nil))
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)

			if test.want != nil {
				assert.Equal(t, test.want, cfg.TokenExchangeAllowedTokenURLs)
			}
		})
	}
}

func TestUserIssuerURLsListsEveryUserIssuer(t *testing.T) {
	content := "databaseURL: " + validDatabaseURL + "\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n" +
		"oidcAdditionalIssuers:\n  - {issuerURL: \"https://idp.example/app/a/\", audience: a}\n  - {issuerURL: \"https://idp.example/app/b/\", audience: b}\n"

	cfg, err := config.Load(writeFile(t, content), envFrom(nil))
	require.NoError(t, err)

	assert.Equal(t, []string{"https://idp.example", "https://idp.example/app/a/", "https://idp.example/app/b/"}, cfg.UserIssuerURLs())
}
