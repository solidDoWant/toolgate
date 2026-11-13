package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func loadTokenExchanges(t *testing.T, keyset bool, exchanges string) (config.Config, error) {
	t.Helper()

	content := "databaseURL: " + validDatabaseURL + "\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\n"
	if keyset {
		content += "secretsKeysetFile: /keys/keyset.json\n"
	}

	path := writeFile(t, content+"tokenExchanges:\n"+exchanges)

	return config.Load(path, envFrom(nil))
}

func exchangeLine(fields string) string {
	return "  - {name: temporal, tokenURL: \"https://idp.example/token\", clientID: c, clientSecretFile: /s" + fields + "}\n"
}

func TestTokenExchangesConfig(t *testing.T) {
	many := strings.Builder{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		many.WriteString("  - {name: " + name + ", tokenURL: \"https://idp.example/token\", clientID: c, clientSecretFile: /s}\n")
	}

	eight := strings.Join(strings.Split(many.String(), "\n")[:8], "\n") + "\n"

	tests := []struct {
		name        string
		keyset      bool
		exchanges   string
		wantTimeout time.Duration
		wantProblem string
	}{
		{name: "valid with every field", keyset: true, exchanges: exchangeLine(", audience: a, scope: \"openid x\", requestedTokenType: \"urn:t\", caFile: /ca.pem, timeout: 20s"), wantTimeout: 20 * time.Second},
		{name: "default timeout", keyset: true, exchanges: exchangeLine(""), wantTimeout: 10 * time.Second},
		{name: "timeout of exactly 60s", keyset: true, exchanges: exchangeLine(", timeout: 60s"), wantTimeout: 60 * time.Second},
		{name: "timeout over 60s", keyset: true, exchanges: exchangeLine(", timeout: 60001ms"), wantProblem: "tokenExchanges[0].timeout: must be at most 60s"},
		{name: "http tokenURL", keyset: true, exchanges: "  - {name: t, tokenURL: \"http://idp.example/token\", clientID: c, clientSecretFile: /s}\n", wantProblem: "tokenExchanges[0].tokenURL"},
		{name: "tokenURL without host", keyset: true, exchanges: "  - {name: t, tokenURL: \"https://\", clientID: c, clientSecretFile: /s}\n", wantProblem: "tokenExchanges[0].tokenURL"},
		{name: "bad name", keyset: true, exchanges: "  - {name: Temporal, tokenURL: \"https://idp.example/token\", clientID: c, clientSecretFile: /s}\n", wantProblem: "tokenExchanges[0].name"},
		{name: "duplicate name", keyset: true, exchanges: exchangeLine("") + exchangeLine(""), wantProblem: "tokenExchanges[1].name: \"temporal\" is listed twice"},
		{name: "missing clientID", keyset: true, exchanges: "  - {name: t, tokenURL: \"https://idp.example/token\", clientSecretFile: /s}\n", wantProblem: "tokenExchanges[0].clientID: required"},
		{name: "missing clientSecretFile", keyset: true, exchanges: "  - {name: t, tokenURL: \"https://idp.example/token\", clientID: c}\n", wantProblem: "tokenExchanges[0].clientSecretFile: required"},
		{name: "eight entries", keyset: true, exchanges: eight, wantTimeout: 10 * time.Second},
		{name: "nine entries", keyset: true, exchanges: many.String(), wantProblem: "tokenExchanges: at most 8 items, got 9"},
		{name: "no keyset", exchanges: exchangeLine(""), wantProblem: "tokenExchanges needs secretsKeysetFile: the gateway encrypts each exchanged token on its way to the leader."},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := loadTokenExchanges(t, test.keyset, test.exchanges)
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, cfg.TokenExchanges)
			assert.Equal(t, test.wantTimeout, cfg.TokenExchanges[0].ExchangeTimeout())
		})
	}
}
