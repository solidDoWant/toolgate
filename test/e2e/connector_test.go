//go:build e2e

package e2e

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/wellknown"
)

// connectorRedirect is the connector client's callback. The sign-in stops
// at the redirect to it, so nothing listens there.
const connectorRedirect = "http://127.0.0.1:1/callback"

// TestConnectorSignIn reaches the gateway at its public URL, so the
// default mcpPublicURL (publicURL + /mcp) is the resource clients use. A
// client following the MCP authorization spec finds the IdP through the
// protected resource metadata, signs in with the connector client using
// PKCE, and connects with the access token.
func TestConnectorSignIn(t *testing.T) {
	provider := oidcProvider(t)
	admin := adminClient(t)

	publicURL := webUI(t, forwardToGateway(t, admin, gatewayPods(t, admin)[0]))
	resource := publicURL + gateway.MCPPath
	metadataURL := publicURL + wellknown.ProtectedResourcePath

	// The web UI's TLS proxy has a test certificate.
	insecure := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // The test's own TLS proxy on loopback.
	client := &http.Client{Transport: insecure}

	t.Run("an MCP request with no token gets 401 naming the protected resource metadata", func(t *testing.T) {
		response := postMCP(t, client, resource, "")
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, `Bearer resource_metadata="`+metadataURL+`"`, response.Header.Get("WWW-Authenticate"))

		response = postMCP(t, client, resource, "not-a-token")
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, `Bearer resource_metadata="`+metadataURL+`", error="invalid_token"`, response.Header.Get("WWW-Authenticate"))
	})

	t.Run("the metadata document names the IdP as the authorization server", func(t *testing.T) {
		for _, target := range []string{metadataURL, publicURL + wellknown.ProtectedResourceRootPath} {
			metadata := getResourceMetadata(t, client, target)
			assert.Equal(t, resource, metadata.Resource, target)
			assert.Equal(t, []string{provider.Issuer}, metadata.AuthorizationServers, target)
		}
	})

	t.Run("a client following the MCP authorization spec signs the user in and acts as them", func(t *testing.T) {
		user := testutil.OIDCUser{Subject: "connector-" + rand.String(8), Email: "connector@example.com", EmailVerified: true}
		session := "e2e-" + rand.String(8)

		// Find the authorization server the way the MCP authorization spec
		// says: the 401's WWW-Authenticate header names the metadata
		// document, and the document names the IdP.
		unauthorized := postMCP(t, client, resource, "")
		challenges, err := oauthex.ParseWWWAuthenticate(unauthorized.Header.Values("WWW-Authenticate"))
		require.NoError(t, err)
		require.Len(t, challenges, 1)
		assert.Equal(t, "bearer", challenges[0].Scheme)

		metadata := getResourceMetadata(t, client, challenges[0].Params["resource_metadata"])
		require.Len(t, metadata.AuthorizationServers, 1)

		identityProvider, err := oidc.NewProvider(t.Context(), metadata.AuthorizationServers[0])
		require.NoError(t, err)

		config := oauth2.Config{
			ClientID:    provider.ConnectorClientID,
			Endpoint:    identityProvider.Endpoint(),
			RedirectURL: connectorRedirect,
			Scopes:      []string{oidc.ScopeOpenID, "email", "profile"},
		}
		verifier := oauth2.GenerateVerifier()
		state := rand.String(16)
		resourceParameter := oauth2.SetAuthURLParam("resource", metadata.Resource)

		code := connectorSignIn(t, user,
			config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), resourceParameter), state)

		token, err := config.Exchange(t.Context(), code, oauth2.VerifierOption(verifier), resourceParameter)
		require.NoError(t, err)

		mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-connector", Version: "0"}, nil)
		connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint: resource,
			HTTPClient: &http.Client{Transport: headerTransport{
				header: http.Header{
					auth.SessionHeader: {session},
					"Authorization":    {"Bearer " + token.AccessToken},
				},
				next: insecure,
			}},
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = connected.Close() })

		tools, err := connected.ListTools(t.Context(), nil)
		require.NoError(t, err)
		assert.NotEmpty(t, tools.Tools)

		require.Eventually(t, func() bool {
			return strings.TrimSpace(gatewayQuery(t, admin, "SELECT principal FROM audit_events WHERE session = '"+session+
				"' AND type = 'session.start' LIMIT 1")) == user.Subject
		}, time.Minute, time.Second, "the session is the signed-in user's")
	})
}

// headerTransport adds header to every request and sends it with next.
type headerTransport struct {
	header http.Header
	next   http.RoundTripper
}

func (h headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range h.header {
		request.Header[name] = values
	}

	return h.next.RoundTrip(request)
}

// postMCP posts an initialize request to the MCP endpoint, with token as the
// bearer token unless it is empty, and returns the closed response.
func postMCP(t *testing.T, client *http.Client, endpoint, token string) *http.Response {
	t.Helper()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(auth.SessionHeader, "e2e-"+rand.String(8))

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := client.Do(request)
	require.NoError(t, err)

	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())

	return response
}

func getResourceMetadata(t *testing.T, client *http.Client, target string) oauthex.ProtectedResourceMetadata {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)

	response, err := client.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode, target)

	var metadata oauthex.ProtectedResourceMetadata
	require.NoError(t, json.NewDecoder(io.LimitReader(response.Body, wellknown.MaxDocumentBytes)).Decode(&metadata))

	return metadata
}

// connectorSignIn acts as a connector's browser: it opens authorizationURL,
// signs in at the fake IdP's sign-in page as user, and returns the code from
// the redirect to connectorRedirect after checking its state.
func connectorSignIn(t *testing.T, user testutil.OIDCUser, authorizationURL, state string) string {
	t.Helper()

	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	redirect := func(method, target string, form url.Values) *url.URL {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}

		request, err := http.NewRequestWithContext(t.Context(), method, target, body)
		require.NoError(t, err)

		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}

		response, err := browser.Do(request)
		require.NoError(t, err)

		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()

		location, err := response.Location()
		require.NoError(t, err, "%s %s redirects", method, target)

		return location
	}

	page := redirect(http.MethodGet, authorizationURL, nil)
	next := redirect(http.MethodPost, page.String(), url.Values{
		"authRequestID":  {page.Query().Get("authRequestID")},
		"subject":        {user.Subject},
		"email":          {user.Email},
		"email_verified": {map[bool]string{true: "true", false: "false"}[user.EmailVerified]},
		"groups":         {strings.Join(user.Groups, ",")},
	})
	callback := redirect(http.MethodGet, next.String(), nil)

	require.True(t, strings.HasPrefix(callback.String(), connectorRedirect), "the IdP redirects to the connector's callback")
	require.Equal(t, state, callback.Query().Get("state"))

	code := callback.Query().Get("code")
	require.NotEmpty(t, code)

	return code
}
