//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/wellknown"
)

// TestBrowserSignsInThroughGatewayIdP proves the browser test harness: it
// finds the IdP from the gateway's client metadata, opens the IdP's sign-in
// page in headless Chromium, signs in as a chosen user with chosen groups,
// and gets an ID token for them through the UI client.
func TestBrowserSignsInThroughGatewayIdP(t *testing.T) {
	provider := oidcProvider(t)
	browser := testutil.StartBrowser(t)

	admin := adminClient(t)
	gatewayURL := strings.TrimSuffix(forwardToGateway(t, admin, gatewayPods(t, admin)[0]), gateway.MCPPath)

	response, err := http.Get(gatewayURL + wellknown.Path)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)

	var metadata wellknown.ClientMetadata
	require.NoError(t, json.NewDecoder(io.LimitReader(response.Body, wellknown.MaxDocumentBytes)).Decode(&metadata))
	require.Equal(t, provider.Issuer, metadata.Issuer, "the gateway trusts the fake identity provider")

	discovered, err := oidc.NewProvider(t.Context(), metadata.Issuer)
	require.NoError(t, err)

	config := oauth2.Config{
		ClientID:    provider.UIClientID,
		Endpoint:    discovered.Endpoint(),
		RedirectURL: gatewayURL + "/ui/auth/callback",
		Scopes:      []string{oidc.ScopeOpenID, "email", "profile"},
	}
	verifier := oauth2.GenerateVerifier()
	user := testutil.OIDCUser{Subject: "browser-" + strings.ToLower(t.Name()), Email: "browser@example.com", EmailVerified: true, Groups: []string{"browser-testers", "developers"}}

	require.NoError(t, browserDo(browser, testutil.Navigate(config.AuthCodeURL("browser-state", oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "login")))))

	title, err := browserRun(browser, chromedp.Title())
	require.NoError(t, err)
	assert.Equal(t, "Sign in to the fake identity provider", title, "the browser shows the IdP's sign-in page")

	// The gateway did not issue this state, so it answers the callback by
	// starting a new sign-in and the browser does not stay on the callback
	// URL. The test reads the callback from the request the browser sends.
	callbacks := make(chan string, 1)

	require.NoError(t, chromedp.Listen(browser, func(event any) {
		if sent, ok := event.(*network.EventRequestWillBeSent); ok && strings.HasPrefix(sent.Request.URL, config.RedirectURL) {
			select {
			case callbacks <- sent.Request.URL:
			default:
			}
		}
	}))

	require.NoError(t, browserDo(browser, testutil.SignInToOIDC(user)))

	var location string

	select {
	case location = <-callbacks:
	case <-time.After(time.Minute):
		require.FailNow(t, "the IdP sends the browser back to the gateway")
	}

	callback, err := url.Parse(location)
	require.NoError(t, err)
	require.Equal(t, "browser-state", callback.Query().Get("state"))

	token, err := config.Exchange(t.Context(), callback.Query().Get("code"), oauth2.VerifierOption(verifier))
	require.NoError(t, err)

	rawIDToken, ok := token.Extra("id_token").(string)
	require.True(t, ok, "the token response has an ID token")

	idToken, err := discovered.Verifier(&oidc.Config{ClientID: config.ClientID}).Verify(t.Context(), rawIDToken)
	require.NoError(t, err)

	var claims struct {
		Subject string   `json:"sub"`
		Groups  []string `json:"groups"`
	}
	require.NoError(t, idToken.Claims(&claims))
	assert.Equal(t, user.Subject, claims.Subject)
	assert.Equal(t, user.Groups, claims.Groups)
}
