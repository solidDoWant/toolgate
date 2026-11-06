//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/wellknown"
)

// connectorRedirect is the connector client's callback. The sign-in stops
// at the redirect to it, so nothing listens there.
const connectorRedirect = "http://127.0.0.1:1/callback"

// TestAC4_09ConnectorSignIn reaches the gateway at its public URL, so the
// default mcpPublicURL (publicURL + /mcp) is the resource clients use. A
// go-sdk client with the AuthorizationCodeHandler finds the IdP through the
// protected resource metadata and signs in with the connector client.
func TestAC4_09ConnectorSignIn(t *testing.T) {
	provider := oidcProvider(t)
	admin := adminClient(t)

	publicURL := webUI(t, forwardToGateway(t, admin, gatewayPods(t, admin)[0]))
	resource := publicURL + gateway.MCPPath
	metadataURL := publicURL + wellknown.ProtectedResourcePath

	// The web UI's TLS proxy has a test certificate.
	insecure := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // The test's own TLS proxy on loopback.
	client := &http.Client{Transport: insecure}

	t.Run("[AC-4-09] an MCP request with no token gets 401 naming the protected resource metadata", func(t *testing.T) {
		response := postMCP(t, client, resource, "")
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, `Bearer resource_metadata="`+metadataURL+`"`, response.Header.Get("WWW-Authenticate"))

		response = postMCP(t, client, resource, "not-a-token")
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, `Bearer resource_metadata="`+metadataURL+`", error="invalid_token"`, response.Header.Get("WWW-Authenticate"))
	})

	t.Run("[AC-4-09] the metadata document names the IdP as the authorization server", func(t *testing.T) {
		for _, target := range []string{metadataURL, publicURL + wellknown.ProtectedResourceRootPath} {
			metadata := getResourceMetadata(t, client, target)
			assert.Equal(t, resource, metadata.Resource, target)
			assert.Equal(t, []string{provider.Issuer}, metadata.AuthorizationServers, target)
		}
	})

	t.Run("[AC-4-09] a client following the MCP authorization spec signs the user in and acts as them", func(t *testing.T) {
		user := testutil.OIDCUser{Subject: "connector-" + rand.String(8), Email: "connector@example.com", EmailVerified: true}
		session := "e2e-" + rand.String(8)

		handler, err := mcpauth.NewAuthorizationCodeHandler(&mcpauth.AuthorizationCodeHandlerConfig{
			PreregisteredClient:      &oauthex.ClientCredentials{ClientID: provider.ConnectorClientID},
			RedirectURL:              connectorRedirect,
			AuthorizationCodeFetcher: connectorSignIn(t, user),
			AcceptUnadvertisedIss:    true,
			ScopeFilter:              func([]string) []string { return []string{oidc.ScopeOpenID, "email", "profile"} },
			Client:                   client,
		})
		require.NoError(t, err)

		mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-connector", Version: "0"}, nil)
		connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint: resource,
			HTTPClient: &http.Client{Transport: headerTransport{
				header: http.Header{auth.SessionHeader: {session}},
				next:   insecure,
			}},
			OAuthHandler: handler,
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

// connectorSignIn returns the authorization code fetcher of a connector: it
// opens the authorization URL, signs in at the fake IdP's sign-in page as
// user, and reads the code from the redirect to connectorRedirect.
func connectorSignIn(t *testing.T, user testutil.OIDCUser) mcpauth.AuthorizationCodeFetcher {
	t.Helper()

	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	redirect := func(ctx context.Context, method, target string, form url.Values) (*url.URL, error) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}

		request, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, err
		}

		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}

		response, err := browser.Do(request)
		if err != nil {
			return nil, err
		}

		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()

		return response.Location()
	}

	return func(ctx context.Context, args *mcpauth.AuthorizationArgs) (*mcpauth.AuthorizationResult, error) {
		page, err := redirect(ctx, http.MethodGet, args.URL, nil)
		if err != nil {
			return nil, err
		}

		next, err := redirect(ctx, http.MethodPost, page.String(), url.Values{
			"authRequestID":  {page.Query().Get("authRequestID")},
			"subject":        {user.Subject},
			"email":          {user.Email},
			"email_verified": {map[bool]string{true: "true", false: "false"}[user.EmailVerified]},
			"groups":         {strings.Join(user.Groups, ",")},
		})
		if err != nil {
			return nil, err
		}

		callback, err := redirect(ctx, http.MethodGet, next.String(), nil)
		if err != nil {
			return nil, err
		}

		assert.True(t, strings.HasPrefix(callback.String(), connectorRedirect), "the IdP redirects to the connector's callback")

		return &mcpauth.AuthorizationResult{
			Code:  callback.Query().Get("code"),
			State: callback.Query().Get("state"),
			Iss:   callback.Query().Get("iss"),
		}, nil
	}
}
