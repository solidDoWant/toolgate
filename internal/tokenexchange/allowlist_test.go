package tokenexchange_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/tokenexchange"
)

func TestStaticAllowListMatchesExactURLsOnly(t *testing.T) {
	list := tokenexchange.StaticAllowList{"https://IdP.Example.com/application/o/token/"}

	tests := []struct {
		name     string
		tokenURL string
		want     bool
	}{
		{name: "same URL", tokenURL: "https://IdP.Example.com/application/o/token/", want: true},
		{name: "scheme and host in another case", tokenURL: "HTTPS://idp.example.com/application/o/token/", want: true},
		{name: "path in another case", tokenURL: "https://idp.example.com/Application/o/token/"},
		{name: "without the trailing slash", tokenURL: "https://idp.example.com/application/o/token"},
		{name: "longer path with the same prefix", tokenURL: "https://idp.example.com/application/o/token/evil"},
		{name: "host with the same prefix", tokenURL: "https://idp.example.com.evil.example/application/o/token/"},
		{name: "another port", tokenURL: "https://idp.example.com:8443/application/o/token/"},
		{name: "query added", tokenURL: "https://idp.example.com/application/o/token/?x=1"},
		{name: "http", tokenURL: "http://idp.example.com/application/o/token/"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := list.Allowed(t.Context(), test.tokenURL)
			require.NoError(t, err)
			assert.Equal(t, test.want, allowed)
		})
	}
}

func TestDiscoveredAllowListHoldsEveryIssuersTokenEndpoint(t *testing.T) {
	endpoints := map[string]string{
		"https://idp.example/main":  "https://idp.example/main/token",
		"https://idp.example/other": "https://idp.example/other/token",
	}

	calls := map[string]int{}
	list := &tokenexchange.DiscoveredAllowList{
		Issuers: []string{"https://idp.example/main", "https://idp.example/other"},
		Discover: func(_ context.Context, issuer string) (string, error) {
			calls[issuer]++

			return endpoints[issuer], nil
		},
	}

	for _, tokenURL := range []string{"https://idp.example/main/token", "https://idp.example/other/token"} {
		allowed, err := list.Allowed(t.Context(), tokenURL)
		require.NoError(t, err)
		assert.True(t, allowed, tokenURL)
	}

	allowed, err := list.Allowed(t.Context(), "https://idp.example/main")
	require.NoError(t, err)
	assert.False(t, allowed, "the issuer URL itself isn't a token endpoint")

	assert.Equal(t, map[string]int{"https://idp.example/main": 1, "https://idp.example/other": 1}, calls, "each issuer is discovered once")
}

func TestDiscoveredAllowListRetriesAFailedIssuer(t *testing.T) {
	down := true
	list := &tokenexchange.DiscoveredAllowList{
		Issuers: []string{"https://idp.example/main", "https://idp.example/down"},
		Discover: func(_ context.Context, issuer string) (string, error) {
			if issuer == "https://idp.example/down" && down {
				return "", errors.New("connection refused")
			}

			return issuer + "/token", nil
		},
	}

	allowed, err := list.Allowed(t.Context(), "https://idp.example/main/token")
	require.NoError(t, err, "an issuer that is down doesn't block the others")
	assert.True(t, allowed)

	_, err = list.Allowed(t.Context(), "https://idp.example/down/token")
	require.ErrorContains(t, err, "https://idp.example/down")

	down = false

	allowed, err = list.Allowed(t.Context(), "https://idp.example/down/token")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDiscoveredAllowListReadsTheDiscoveryDocument(t *testing.T) {
	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	list := &tokenexchange.DiscoveredAllowList{Issuers: []string{provider.Issuer}}

	allowed, err := list.Allowed(t.Context(), provider.TokenURL())
	require.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = list.Allowed(t.Context(), provider.Issuer+"/userinfo")
	require.NoError(t, err)
	assert.False(t, allowed)
}
