package testutil_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

// TestOIDCProviderConnectorClient runs the go-sdk's AuthorizationCodeHandler
// against the fake provider's connector client: the handler finds the
// provider through protected resource metadata, uses PKCE and gets a token
// whose audience is the connector client ID.
func TestOIDCProviderConnectorClient(t *testing.T) {
	flow := newUIFlow(t, testutil.OIDCOptions{})
	provider := flow.provider

	var resourceURL string

	resource := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/.well-known/oauth-protected-resource" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(oauthex.ProtectedResourceMetadata{
				Resource:             resourceURL,
				AuthorizationServers: []string{provider.Issuer},
			})

			return
		}

		writer.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+resourceURL+`/.well-known/oauth-protected-resource"`)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(resource.Close)

	resourceURL = resource.URL

	redirect := "http://127.0.0.1:1/callback"

	fetch := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		page := mustGet(t, flow, args.URL)
		require.Equal(t, testutil.OIDCSignInPath, page.Path, page.String())

		callback := flow.signInTo(t, page, alice, redirect)

		return &auth.AuthorizationResult{
			Code:  callback.Query().Get("code"),
			State: callback.Query().Get("state"),
			Iss:   callback.Query().Get("iss"),
		}, nil
	}

	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      &oauthex.ClientCredentials{ClientID: provider.ConnectorClientID},
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: fetch,
		AcceptUnadvertisedIss:    true,
		ScopeFilter:              func([]string) []string { return []string{oidc.ScopeOpenID, "email", "profile"} },
	})
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, resource.URL, nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)

	require.NoError(t, handler.Authorize(t.Context(), request, response))

	source, err := handler.TokenSource(t.Context())
	require.NoError(t, err)

	token, err := source.Token()
	require.NoError(t, err)

	discovered, err := oidc.NewProvider(t.Context(), provider.Issuer)
	require.NoError(t, err)

	verified, err := discovered.Verifier(&oidc.Config{ClientID: provider.ConnectorClientID}).Verify(t.Context(), token.AccessToken)
	require.NoError(t, err, "the access token's audience is the connector client ID")
	assert.Equal(t, "alice", verified.Subject)
}

func TestOIDCProviderConnectorClientRequiresPKCE(t *testing.T) {
	flow := newUIFlow(t, testutil.OIDCOptions{})
	flow.config.ClientID = flow.provider.ConnectorClientID
	flow.config.RedirectURL = "http://127.0.0.1:1/callback"

	response, _ := flow.get(t, flow.config.AuthCodeURL("state-1"))
	require.Equal(t, http.StatusFound, response.StatusCode)

	location, err := response.Location()
	require.NoError(t, err)
	assert.Equal(t, "invalid_request", location.Query().Get("error"))
}

// mustGet requests the authorization URL and returns the sign-in page it
// redirects to.
func mustGet(t *testing.T, flow *uiFlow, target string) *url.URL {
	t.Helper()

	response, body := flow.get(t, target)
	require.Equal(t, http.StatusFound, response.StatusCode, target+" "+body)

	location, err := response.Location()
	require.NoError(t, err)

	return location
}
