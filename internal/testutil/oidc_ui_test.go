package testutil_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

// uiFlow runs the UI client's authorization-code flow against a fake
// provider with plain HTTP requests, standing in for a browser.
type uiFlow struct {
	provider *testutil.OIDCProvider
	config   oauth2.Config
	client   *http.Client
}

func newUIFlow(t *testing.T, options testutil.OIDCOptions) *uiFlow {
	t.Helper()

	provider := testutil.StartOIDCProvider(t, options)

	// The UI's callback; the flow stops at the redirect to it.
	callback := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(callback.Close)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &uiFlow{
		provider: provider,
		config: oauth2.Config{
			ClientID:    provider.UIClientID,
			Endpoint:    oauth2.Endpoint{AuthURL: provider.AuthorizationURL(), TokenURL: provider.TokenURL(), AuthStyle: oauth2.AuthStyleInParams},
			RedirectURL: callback.URL + "/ui/auth/callback",
			Scopes:      []string{oidc.ScopeOpenID, "email", "profile"},
		},
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// get requests target and returns the response, closed.
func (f *uiFlow) get(t *testing.T, target string) (*http.Response, string) {
	t.Helper()

	response, err := f.client.Get(target)
	require.NoError(t, err)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	return response, string(body)
}

// authorize starts a sign-in and follows the provider's redirects until
// either the sign-in form or the client's callback. It returns the
// sign-in page URL and the form, or the callback URL and "".
func (f *uiFlow) authorize(t *testing.T, verifier string, options ...oauth2.AuthCodeOption) (*url.URL, string) {
	t.Helper()

	options = append(options, oauth2.S256ChallengeOption(verifier))
	target := f.config.AuthCodeURL("state-1", options...)

	for range 10 {
		response, body := f.get(t, target)

		switch response.StatusCode {
		case http.StatusOK:
			return response.Request.URL, body
		case http.StatusFound, http.StatusSeeOther:
			location, err := response.Location()
			require.NoError(t, err)

			if strings.HasPrefix(location.String(), f.config.RedirectURL) {
				return location, ""
			}

			target = location.String()
		default:
			require.FailNow(t, "unexpected response", "%s: %d %s", target, response.StatusCode, body)
		}
	}

	require.FailNow(t, "too many redirects")

	return nil, ""
}

// signIn posts the sign-in form shown at page and returns the callback URL.
func (f *uiFlow) signIn(t *testing.T, page *url.URL, user testutil.OIDCUser) *url.URL {
	t.Helper()

	return f.signInTo(t, page, user, f.config.RedirectURL)
}

// signInTo is signIn for a flow whose redirect URL is redirect.
func (f *uiFlow) signInTo(t *testing.T, page *url.URL, user testutil.OIDCUser, redirect string) *url.URL {
	t.Helper()

	form := url.Values{
		"authRequestID":  {page.Query().Get("authRequestID")},
		"subject":        {user.Subject},
		"email":          {user.Email},
		"email_verified": {map[bool]string{true: "true", false: "false"}[user.EmailVerified]},
		"groups":         {strings.Join(user.Groups, ",")},
	}

	response, err := f.client.PostForm(page.String(), form)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusFound, response.StatusCode)

	location, err := response.Location()
	require.NoError(t, err)

	callback, _ := f.followToCallback(t, location.String(), redirect)

	return callback
}

func (f *uiFlow) followToCallback(t *testing.T, target, redirect string) (*url.URL, string) {
	t.Helper()

	response, body := f.get(t, target)
	require.Equal(t, http.StatusFound, response.StatusCode, body)

	location, err := response.Location()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(location.String(), redirect), "redirect to %s", location)

	return location, body
}

// exchange trades the callback's code for tokens and returns the verified
// ID token's claims.
func (f *uiFlow) exchange(t *testing.T, callback *url.URL, verifier string) map[string]any {
	t.Helper()

	require.Equal(t, "state-1", callback.Query().Get("state"))

	token, err := f.config.Exchange(t.Context(), callback.Query().Get("code"), oauth2.VerifierOption(verifier))
	require.NoError(t, err)

	rawIDToken, ok := token.Extra("id_token").(string)
	require.True(t, ok, "the token response has an ID token")

	provider, err := oidc.NewProvider(t.Context(), f.provider.Issuer)
	require.NoError(t, err)

	idToken, err := provider.Verifier(&oidc.Config{ClientID: f.config.ClientID}).Verify(t.Context(), rawIDToken)
	require.NoError(t, err)

	claims := map[string]any{}
	require.NoError(t, idToken.Claims(&claims))

	return claims
}

var alice = testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"developers", "admins"}}

func TestOIDCProviderUIClientSignsInChosenUser(t *testing.T) {
	flow := newUIFlow(t, testutil.OIDCOptions{})
	verifier := oauth2.GenerateVerifier()

	page, form := flow.authorize(t, verifier)
	require.Contains(t, form, `id="subject"`, "the provider shows its sign-in form")
	assert.Equal(t, testutil.OIDCSignInPath, page.Path)

	callback := flow.signIn(t, page, alice)
	claims := flow.exchange(t, callback, verifier)

	assert.Equal(t, "alice", claims["sub"])
	assert.Equal(t, "alice@example.com", claims["email"])
	assert.Equal(t, true, claims["email_verified"])
	assert.Equal(t, []any{"developers", "admins"}, claims["groups"])
	assert.NotZero(t, claims["auth_time"], "the ID token carries auth_time")
}

func TestOIDCProviderUIClientRequiresPKCE(t *testing.T) {
	flow := newUIFlow(t, testutil.OIDCOptions{})

	t.Run("no code challenge", func(t *testing.T) {
		response, _ := flow.get(t, flow.config.AuthCodeURL("state-1"))
		require.Equal(t, http.StatusFound, response.StatusCode)

		location, err := response.Location()
		require.NoError(t, err)
		assert.Equal(t, "invalid_request", location.Query().Get("error"), "an authorization request without PKCE is refused")
	})

	t.Run("wrong verifier", func(t *testing.T) {
		page, _ := flow.authorize(t, oauth2.GenerateVerifier())
		callback := flow.signIn(t, page, alice)

		_, err := flow.config.Exchange(t.Context(), callback.Query().Get("code"), oauth2.VerifierOption(oauth2.GenerateVerifier()))
		require.Error(t, err, "a code exchange with the wrong verifier is refused")
	})
}

func TestOIDCProviderUIClientSession(t *testing.T) {
	tests := []struct {
		name     string
		options  []oauth2.AuthCodeOption
		wantForm bool
	}{
		{name: "no prompt reuses the session", wantForm: false},
		{name: "a long max_age reuses the session", options: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("max_age", "3600")}, wantForm: false},
		{name: "prompt=login signs in again", options: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("prompt", "login")}, wantForm: true},
		{name: "max_age=0 signs in again", options: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("max_age", "0")}, wantForm: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			flow := newUIFlow(t, testutil.OIDCOptions{})

			verifier := oauth2.GenerateVerifier()
			page, _ := flow.authorize(t, verifier)
			first := flow.exchange(t, flow.signIn(t, page, alice), verifier)

			verifier = oauth2.GenerateVerifier()
			target, form := flow.authorize(t, verifier, test.options...)

			if test.wantForm {
				require.Contains(t, form, `id="subject"`, "the provider asks the user to sign in again")

				return
			}

			require.Empty(t, form, "the provider signs the browser in without the form")

			second := flow.exchange(t, target, verifier)
			assert.Equal(t, "alice", second["sub"])
			assert.Equal(t, first["auth_time"], second["auth_time"], "a reused session keeps its auth_time")
		})
	}
}

func TestOIDCProviderOmitAuthTime(t *testing.T) {
	flow := newUIFlow(t, testutil.OIDCOptions{OmitAuthTime: true})
	verifier := oauth2.GenerateVerifier()

	page, _ := flow.authorize(t, verifier, oauth2.SetAuthURLParam("max_age", "0"))
	claims := flow.exchange(t, flow.signIn(t, page, alice), verifier)

	assert.Equal(t, "alice", claims["sub"])
	assert.NotContains(t, claims, "auth_time")
}
