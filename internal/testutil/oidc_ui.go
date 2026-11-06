package testutil

import (
	"context"
	"crypto/rand"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zitadel/oidc/v3/example/server/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// Defaults of the fake OIDC provider's UI client.
const (
	DefaultOIDCUIClientID = "toolgate-ui"
	// DefaultOIDCUIRedirectURI matches the UI's callback on any loopback
	// port, as a test reaches the gateway through a port-forward.
	DefaultOIDCUIRedirectURI = "http://127.0.0.1:*/ui/auth/callback"
)

// Defaults of the fake OIDC provider's connector client.
const (
	DefaultOIDCConnectorClientID = "toolgate-connector"
	// DefaultOIDCConnectorRedirectURI matches a connector test's callback
	// on any loopback port.
	DefaultOIDCConnectorRedirectURI = "http://127.0.0.1:*/callback"
)

// OIDCSignInPath is the path of the fake provider's sign-in page.
const OIDCSignInPath = "/login"

// The fake provider's browser session cookie.
const oidcSessionCookie = "fakeidp_session"

// The fake provider's sign-in form. Tests fill it through SignInToOIDC.
var oidcSignInPage = template.Must(template.New("sign-in").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Sign in to the fake identity provider</title></head>
<body>
<h1>Sign in to the fake identity provider</h1>
<form method="post" action="">
<input type="hidden" name="authRequestID" value="{{.}}">
<label>Subject <input id="subject" name="subject" required></label>
<label>Email <input id="email" name="email"></label>
<label>Email verified <select id="email_verified" name="email_verified"><option value="true">true</option><option value="false">false</option></select></label>
<label>Groups, separated by commas <input id="groups" name="groups"></label>
<button id="sign-in" type="submit">Sign in</button>
</form>
</body>
</html>
`))

// oidcSession is a sign-in remembered by the provider's session cookie.
type oidcSession struct {
	subject  string
	authTime time.Time
}

// oidcBrowser is the provider's sign-in page and browser sessions. A
// browser with a session is signed in again without the form, with the
// session's auth_time, unless the request has prompt=login or a max_age
// shorter than the session's age.
type oidcBrowser struct {
	issuer  string
	storage *oidcStorage

	mu       sync.Mutex
	sessions map[string]oidcSession
}

func (b *oidcBrowser) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	id := request.Form.Get("authRequestID")

	authRequest, err := b.storage.exampleAuthRequest(request.Context(), id)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	switch request.Method {
	case http.MethodGet:
		if session, ok := b.session(request); ok && sessionSatisfies(session, authRequest) {
			b.complete(writer, request, id, session)

			return
		}

		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		_ = oidcSignInPage.Execute(writer, id)
	case http.MethodPost:
		user := OIDCUser{
			Subject:       strings.TrimSpace(request.PostForm.Get("subject")),
			Email:         strings.TrimSpace(request.PostForm.Get("email")),
			EmailVerified: request.PostForm.Get("email_verified") != "false",
		}
		if user.Subject == "" {
			http.Error(writer, "subject is required", http.StatusBadRequest)

			return
		}

		for group := range strings.SplitSeq(request.PostForm.Get("groups"), ",") {
			if group = strings.TrimSpace(group); group != "" {
				user.Groups = append(user.Groups, group)
			}
		}

		b.storage.users.set(user)

		session := oidcSession{subject: user.Subject, authTime: time.Now()}
		sessionID := rand.Text()

		b.mu.Lock()
		b.sessions[sessionID] = session
		b.mu.Unlock()

		http.SetCookie(writer, &http.Cookie{Name: oidcSessionCookie, Value: sessionID, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		b.complete(writer, request, id, session)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *oidcBrowser) session(request *http.Request) (oidcSession, bool) {
	cookie, err := request.Cookie(oidcSessionCookie)
	if err != nil {
		return oidcSession{}, false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	session, ok := b.sessions[cookie.Value]

	return session, ok
}

// sessionSatisfies reports whether a remembered sign-in may answer the
// request: no prompt=login, and no max_age shorter than the session's age.
// The provider turns prompt=login into max_age=0.
func sessionSatisfies(session oidcSession, authRequest *storage.AuthRequest) bool {
	if slices.Contains(authRequest.Prompt, oidc.PromptLogin) {
		return false
	}

	return authRequest.MaxAuthAge == nil || time.Since(session.authTime) <= *authRequest.MaxAuthAge
}

// complete finishes the authorization request id as session's user and
// sends the browser back to the provider, which redirects to the client.
func (b *oidcBrowser) complete(writer http.ResponseWriter, request *http.Request, id string, session oidcSession) {
	if err := b.storage.completeAuthRequest(id, session); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	http.Redirect(writer, request, b.issuer+"/authorize/callback?id="+url.QueryEscape(id), http.StatusFound)
}

// uiAuthRequest is an authorization request of the example storage with
// the auth_time of the sign-in that completed it, or none when the
// provider leaves auth_time out.
type uiAuthRequest struct {
	*storage.AuthRequest

	authTime time.Time
}

func (r *uiAuthRequest) GetAuthTime() time.Time { return r.authTime }

// unwrapTokenRequest gives the example storage its own request type back:
// it reads the client from it.
func unwrapTokenRequest(request op.TokenRequest) op.TokenRequest {
	if wrapped, ok := request.(*uiAuthRequest); ok {
		return wrapped.AuthRequest
	}

	return request
}

// uiAuthRequests keeps the auth_time of completed authorization requests.
type uiAuthRequests struct {
	mu        sync.Mutex
	authTimes map[string]time.Time
}

func (s *oidcStorage) CreateAuthRequest(ctx context.Context, authRequest *oidc.AuthRequest, userID string) (op.AuthRequest, error) {
	if (authRequest.ClientID == s.uiClient.id || authRequest.ClientID == s.connectorClient.id) && authRequest.CodeChallenge == "" {
		return nil, oidc.ErrInvalidRequest().WithDescription("the authorization-code clients must use PKCE (code_challenge)")
	}

	return s.Storage.CreateAuthRequest(ctx, authRequest, userID)
}

func (s *oidcStorage) AuthRequestByID(ctx context.Context, id string) (op.AuthRequest, error) {
	return s.wrapAuthRequest(s.Storage.AuthRequestByID(ctx, id))
}

func (s *oidcStorage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	return s.wrapAuthRequest(s.Storage.AuthRequestByCode(ctx, code))
}

func (s *oidcStorage) DeleteAuthRequest(ctx context.Context, id string) error {
	s.authRequests.mu.Lock()
	delete(s.authRequests.authTimes, id)
	s.authRequests.mu.Unlock()

	return s.Storage.DeleteAuthRequest(ctx, id)
}

func (s *oidcStorage) CreateAccessToken(ctx context.Context, request op.TokenRequest) (string, time.Time, error) {
	if err := s.subjectReady(ctx, request.GetSubject()); err != nil {
		return "", time.Time{}, err
	}

	return s.Storage.CreateAccessToken(ctx, unwrapTokenRequest(request))
}

func (s *oidcStorage) CreateAccessAndRefreshTokens(ctx context.Context, request op.TokenRequest, currentRefreshToken string) (string, string, time.Time, error) {
	if err := s.subjectReady(ctx, request.GetSubject()); err != nil {
		return "", "", time.Time{}, err
	}

	return s.Storage.CreateAccessAndRefreshTokens(ctx, unwrapTokenRequest(request), currentRefreshToken)
}

func (s *oidcStorage) wrapAuthRequest(request op.AuthRequest, err error) (op.AuthRequest, error) {
	if err != nil {
		return nil, err
	}

	example, ok := request.(*storage.AuthRequest)
	if !ok {
		return request, nil
	}

	wrapped := &uiAuthRequest{AuthRequest: example}

	if !s.omitAuthTime {
		s.authRequests.mu.Lock()
		wrapped.authTime = s.authRequests.authTimes[example.ID]
		s.authRequests.mu.Unlock()
	}

	return wrapped, nil
}

// exampleAuthRequest is the authorization request id as the example
// storage keeps it.
func (s *oidcStorage) exampleAuthRequest(ctx context.Context, id string) (*storage.AuthRequest, error) {
	request, err := s.Storage.AuthRequestByID(ctx, id)
	if err != nil {
		return nil, err
	}

	example, ok := request.(*storage.AuthRequest)
	if !ok {
		return nil, errors.New("unexpected authorization request type")
	}

	return example, nil
}

// completeAuthRequest marks the authorization request id done for the
// session's user and records the session's auth_time for it.
func (s *oidcStorage) completeAuthRequest(id string, session oidcSession) error {
	// The example storage's users have an empty password; this sets the
	// request's user and marks it done.
	if err := s.CheckUsernamePassword(session.subject, "", id); err != nil {
		return err
	}

	s.authRequests.mu.Lock()
	s.authRequests.authTimes[id] = session.authTime
	s.authRequests.mu.Unlock()

	return nil
}

// uiClient is the UI's public authorization-code client. Its redirect URIs
// are globs, so a test can use a port-forward's random port.
type uiClient struct {
	id           string
	redirectURIs []string
	loginURL     string
}

func (c *uiClient) GetID() string                        { return c.id }
func (c *uiClient) RedirectURIs() []string               { return nil }
func (c *uiClient) RedirectURIGlobs() []string           { return c.redirectURIs }
func (c *uiClient) PostLogoutRedirectURIs() []string     { return nil }
func (c *uiClient) PostLogoutRedirectURIGlobs() []string { return nil }
func (c *uiClient) ApplicationType() op.ApplicationType  { return op.ApplicationTypeWeb }
func (c *uiClient) AuthMethod() oidc.AuthMethod          { return oidc.AuthMethodNone }
func (c *uiClient) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}

func (c *uiClient) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode}
}

func (c *uiClient) LoginURL(id string) string {
	return c.loginURL + "?authRequestID=" + url.QueryEscape(id)
}
func (c *uiClient) AccessTokenType() op.AccessTokenType { return op.AccessTokenTypeJWT }
func (c *uiClient) IDTokenLifetime() time.Duration      { return 5 * time.Minute }

// DevMode allows the http redirect URIs of the test environment.
func (c *uiClient) DevMode() bool                        { return true }
func (c *uiClient) IsScopeAllowed(string) bool           { return false }
func (c *uiClient) IDTokenUserinfoClaimsAssertion() bool { return true }
func (c *uiClient) ClockSkew() time.Duration             { return 0 }

func (c *uiClient) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}

func (c *uiClient) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}
