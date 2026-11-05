//go:build integration

package ui_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

const whoamiPath = "/ui/whoami"

// refusals records the auth.refuse events of failed sign-ins.
type refusals struct {
	mu     sync.Mutex
	events []auth.Refusal
}

func (r *refusals) RecordAuthRefusal(_ context.Context, refusal auth.Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, refusal)
}

func (r *refusals) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	reasons := make([]string, 0, len(r.events))
	for _, event := range r.events {
		reasons = append(reasons, event.Reason)
	}

	return reasons
}

// replica is one gateway's web UI.
type replica struct {
	ui  *ui.UI
	mux *muxRegistrar
}

// uiEnv is two gateway replicas behind one load balancer that sends each
// request to the replica the test picks, sharing one database and one IdP.
type uiEnv struct {
	pool     *pgxpool.Pool
	idp      *toolgatetest.OIDCProvider
	server   *httptest.Server
	replicas []replica
	target   atomic.Int32
	refusals *refusals
	servers  *fakeServers
	rules    *fakeRules
	// formSubmits counts the step-up form's POSTs that ran.
	formSubmits atomic.Int32
	broker      *secrets.Broker
}

func startUIEnv(t *testing.T, adminGroup string) *uiEnv {
	t.Helper()

	return startUIEnvWithIdP(t, adminGroup, toolgatetest.OIDCOptions{})
}

// startUIEnvWithIdP is startUIEnv with an IdP configured by idpOptions.
func startUIEnvWithIdP(t *testing.T, adminGroup string, idpOptions toolgatetest.OIDCOptions) *uiEnv {
	t.Helper()

	postgres := toolgatetest.StartPostgres(t)

	pool, err := pgxpool.New(t.Context(), postgres.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	env := &uiEnv{pool: pool, refusals: &refusals{}, servers: &fakeServers{}, rules: &fakeRules{}}

	idpOptions.UIRedirectURIs = []string{"https://127.0.0.1:*/ui/auth/callback"}
	env.idp = toolgatetest.StartOIDCProvider(t, idpOptions)

	env.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		env.replicas[env.target.Load()].mux.mux.ServeHTTP(writer, request)
	}))

	env.server.StartTLS()
	t.Cleanup(env.server.Close)

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	cipher, err := secrets.NewCipher(handle)
	require.NoError(t, err)

	broker := secrets.NewBroker(db.New(pool), cipher)
	env.broker = broker

	// The public URL is only known once the server listens.
	for range 2 {
		registrar := newMuxRegistrar()
		webUI := ui.New(ui.Options{
			Database:   pool,
			IssuerURL:  env.idp.Issuer,
			ClientID:   env.idp.UIClientID,
			Scopes:     []string{"openid", "email", "profile"},
			PublicURL:  env.server.URL,
			AdminGroup: adminGroup,
			Recorder:   env.refusals,
			Servers:    env.servers,
			Pool:       pool,
			Rules:      env.rules,
			Secrets:    broker,
			Auditor:    audit.NewRecorder(io.Discard, db.New(pool)),
		}, registrar)

		webUI.Handle("GET "+whoamiPath, ui.User, http.HandlerFunc(whoami))
		webUI.Handle("GET /ui/admin-only", ui.Admin, http.HandlerFunc(whoami))
		webUI.Handle("POST /ui/admin-only", ui.Admin, http.HandlerFunc(whoami))
		registerStepUpForm(webUI, &env.formSubmits)

		env.replicas = append(env.replicas, replica{ui: webUI, mux: registrar})
	}

	return env
}

func whoami(writer http.ResponseWriter, request *http.Request) {
	session, ok := ui.SessionFrom(request.Context())
	if !ok {
		http.Error(writer, "no session", http.StatusInternalServerError)

		return
	}

	_ = json.NewEncoder(writer).Encode(map[string]any{
		"principal": session.Principal,
		"email":     session.Email,
		"groups":    session.Groups,
		"admin":     session.Admin,
		"authTime":  !session.AuthTime.IsZero(),
	})
}

// browser is an HTTP client with its own cookies that doesn't follow
// redirects, so each step of a sign-in can be checked.
func (e *uiEnv) browser(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{
		Transport: e.server.Client().Transport,
		Jar:       jar,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func get(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)

	response, err := client.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

func post(t *testing.T, client *http.Client, target string, form url.Values) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := client.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

func location(t *testing.T, response *http.Response) *url.URL {
	t.Helper()

	target, err := response.Location()
	require.NoError(t, err, "status %d", response.StatusCode)

	return target
}

// setCookie returns the cookie the response sets whose name is name, or
// starts with name when it ends with a dash.
func setCookie(response *http.Response, name string) *http.Cookie {
	for _, cookie := range response.Cookies() {
		if cookie.Name == name || (strings.HasSuffix(name, "-") && strings.HasPrefix(cookie.Name, name)) {
			return cookie
		}
	}

	return nil
}

// startLogin opens `/ui/login` and returns the IdP's authorization URL.
func (e *uiEnv) startLogin(t *testing.T, client *http.Client, returnTo string) (*url.URL, *http.Response) {
	t.Helper()

	response := get(t, client, e.server.URL+ui.LoginPath+"?"+url.Values{"return_to": {returnTo}}.Encode())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	return location(t, response), response
}

// signInAtIdP signs client in at the fake IdP as user and returns the
// gateway callback URL the IdP redirects to, without opening it.
func (e *uiEnv) signInAtIdP(t *testing.T, client *http.Client, authURL *url.URL, user toolgatetest.OIDCUser) *url.URL {
	t.Helper()

	target := authURL

	for range 10 {
		response := get(t, client, target.String())
		if response.StatusCode == http.StatusOK {
			break
		}

		require.Contains(t, []int{http.StatusFound, http.StatusSeeOther}, response.StatusCode)
		target = target.ResolveReference(location(t, response))
	}

	require.Equal(t, toolgatetest.OIDCSignInPath, target.Path, "the IdP shows its sign-in page")

	emailVerified := "false"
	if user.EmailVerified {
		emailVerified = "true"
	}

	response := post(t, client, target.String(), url.Values{
		"authRequestID":  {target.Query().Get("authRequestID")},
		"subject":        {user.Subject},
		"email":          {user.Email},
		"email_verified": {emailVerified},
		"groups":         {strings.Join(user.Groups, ",")},
	})

	for range 10 {
		require.Contains(t, []int{http.StatusFound, http.StatusSeeOther}, response.StatusCode)

		next := target.ResolveReference(location(t, response))
		if next.Path == ui.CallbackPath {
			return next
		}

		target = next
		response = get(t, client, target.String())
	}

	require.FailNow(t, "the IdP never redirected to the callback")

	return nil
}

func (e *uiEnv) whoami(t *testing.T, client *http.Client) map[string]any {
	t.Helper()

	response := get(t, client, e.server.URL+whoamiPath)
	require.Equal(t, http.StatusOK, response.StatusCode)

	var identity map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&identity))

	return identity
}

func (e *uiEnv) countRows(t *testing.T, table string) int {
	t.Helper()

	var count int
	require.NoError(t, e.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count))

	return count
}

// [design row 62] A sign-in started on one replica completes on another.
func TestSignInCompletesOnAnotherReplica(t *testing.T) {
	env := startUIEnv(t, "toolgate-admins")
	browser := env.browser(t)

	env.target.Store(0)
	authURL, loginResponse := env.startLogin(t, browser, "/ui/whoami")

	assert.Equal(t, env.idp.Issuer, authURL.Scheme+"://"+authURL.Host)
	assert.Equal(t, "S256", authURL.Query().Get("code_challenge_method"))
	assert.NotEmpty(t, authURL.Query().Get("code_challenge"))
	assert.NotEmpty(t, authURL.Query().Get("nonce"))
	assert.Equal(t, env.server.URL+ui.CallbackPath, authURL.Query().Get("redirect_uri"))
	assert.Equal(t, "openid email profile", authURL.Query().Get("scope"))

	loginCookie := setCookie(loginResponse, ui.LoginCookiePrefix)
	require.NotNil(t, loginCookie)
	assert.Len(t, loginCookie.Name, len(ui.LoginCookiePrefix)+8)
	assert.Equal(t, authURL.Query().Get("state"), loginCookie.Value)
	assert.True(t, loginCookie.Secure)
	assert.True(t, loginCookie.HttpOnly)
	assert.Equal(t, "/", loginCookie.Path)
	assert.Equal(t, http.SameSiteLaxMode, loginCookie.SameSite)
	assert.Equal(t, 600, loginCookie.MaxAge)
	assert.Equal(t, 1, env.countRows(t, "ui_logins"))

	callback := env.signInAtIdP(t, browser, authURL, toolgatetest.OIDCUser{
		Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"toolgate-admins", "dev"},
	})

	env.target.Store(1)

	response := get(t, browser, callback.String())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, "/ui/whoami", location(t, response).Path)

	sessionCookie := setCookie(response, ui.SessionCookie)
	require.NotNil(t, sessionCookie)
	assert.Equal(t, ui.SessionCookie, sessionCookie.Name)
	assert.True(t, sessionCookie.Secure)
	assert.True(t, sessionCookie.HttpOnly)
	assert.Equal(t, "/", sessionCookie.Path)
	assert.Equal(t, http.SameSiteLaxMode, sessionCookie.SameSite)
	assert.Equal(t, 8*60*60, sessionCookie.MaxAge)

	assert.Equal(t, 0, env.countRows(t, "ui_logins"), "the sign-in row is taken")
	assert.Empty(t, env.refusals.reasons())

	for _, target := range []int32{0, 1} {
		env.target.Store(target)

		identity := env.whoami(t, browser)
		assert.Equal(t, "alice", identity["principal"])
		assert.Equal(t, "alice@example.com", identity["email"])
		assert.Equal(t, []any{"toolgate-admins", "dev"}, identity["groups"])
		assert.Equal(t, true, identity["admin"])
		assert.Equal(t, true, identity["authTime"])
	}

	// The stored row holds the hash of the cookie, not the cookie.
	var stored int
	require.NoError(t, env.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM ui_sessions WHERE id_hash = sha256($1::bytea)", []byte(sessionCookie.Value)).Scan(&stored))
	assert.Equal(t, 1, stored)
}

func TestSignInDropsUnverifiedEmail(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	authURL, _ := env.startLogin(t, browser, "/ui/whoami")
	callback := env.signInAtIdP(t, browser, authURL, toolgatetest.OIDCUser{Subject: "bob", Email: "bob@example.com", EmailVerified: false})

	response := get(t, browser, callback.String())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	identity := env.whoami(t, browser)
	assert.Equal(t, "bob", identity["principal"])
	assert.Empty(t, identity["email"])
}

func TestSignInReturnsOnlyToUIPaths(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	authURL, _ := env.startLogin(t, browser, "https://evil.example/ui/whoami")
	callback := env.signInAtIdP(t, browser, authURL, toolgatetest.OIDCUser{Subject: "bob"})

	response := get(t, browser, callback.String())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.HomePath, response.Header.Get("Location"))
}

func TestSignInCallbackFromAnotherBrowserIsRefused(t *testing.T) {
	env := startUIEnv(t, "")
	victim := env.browser(t)
	attacker := env.browser(t)

	// The attacker starts a sign-in and hands its callback to the victim.
	authURL, _ := env.startLogin(t, attacker, "/ui/whoami")
	callback := env.signInAtIdP(t, attacker, authURL, toolgatetest.OIDCUser{Subject: "attacker"})

	response := get(t, victim, callback.String())
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Nil(t, setCookie(response, ui.SessionCookie))
	assert.Equal(t, 0, env.countRows(t, "ui_sessions"))
	assert.Equal(t, []string{"web UI sign-in: the state does not match this browser's sign-in"}, env.refusals.reasons())

	response = get(t, victim, env.server.URL+whoamiPath)
	assert.Equal(t, http.StatusSeeOther, response.StatusCode, "the victim is not signed in")
}

func TestSignInStateWorksOnce(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	authURL, loginResponse := env.startLogin(t, browser, "/ui/whoami")
	loginCookie := setCookie(loginResponse, ui.LoginCookiePrefix)
	require.NotNil(t, loginCookie)

	callback := env.signInAtIdP(t, browser, authURL, toolgatetest.OIDCUser{Subject: "carol"})

	response := get(t, browser, callback.String())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	// Replay the callback with the state cookie put back.
	replay := env.browser(t)
	serverURL, err := url.Parse(env.server.URL)
	require.NoError(t, err)
	replay.Jar.SetCookies(serverURL, []*http.Cookie{{Name: loginCookie.Name, Value: loginCookie.Value, Path: "/", Secure: true}})

	response = get(t, replay, callback.String())
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Nil(t, setCookie(response, ui.SessionCookie))
	assert.Equal(t, []string{"web UI sign-in: the sign-in is unknown, already used or expired"}, env.refusals.reasons())
}

func TestFailedSignInsFollowTheRefusalRateRule(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	for range 11 {
		response := get(t, browser, env.server.URL+ui.CallbackPath+"?state=forged&code=x")
		require.Equal(t, http.StatusForbidden, response.StatusCode)
	}

	assert.Len(t, env.refusals.reasons(), 10)
	assert.Equal(t, 1.0, testutil.ToFloat64(env.replicas[0].ui.DroppedRefusals()))
}

func TestLoginIsRefusedOverPendingLimit(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	insertLogins := func(count int) {
		_, err := env.pool.Exec(t.Context(), `
INSERT INTO ui_logins (state_hash, nonce, pkce_verifier, return_to, step_up)
SELECT sha256(convert_to('filler-' || n, 'UTF8')), 'n', 'v', '/ui/servers', false
FROM generate_series(1, $1) AS n`, count)
		require.NoError(t, err)
	}

	// Exactly the limit still starts a sign-in, which adds one row.
	insertLogins(ui.MaxPendingLogins - 1)

	response := get(t, browser, env.server.URL+ui.LoginPath)
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.MaxPendingLogins, env.countRows(t, "ui_logins"))

	response = get(t, browser, env.server.URL+ui.LoginPath)
	assert.Equal(t, http.StatusSeeOther, response.StatusCode, "at exactly the limit")
	assert.Equal(t, ui.MaxPendingLogins+1, env.countRows(t, "ui_logins"))

	response = get(t, browser, env.server.URL+ui.LoginPath)
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode, "over the limit")
	assert.Equal(t, ui.MaxPendingLogins+1, env.countRows(t, "ui_logins"))
}

// insertSession stores a UI session and returns a browser holding its cookie.
func (e *uiEnv) insertSession(t *testing.T, cookieValue string, groups []string) *http.Client {
	t.Helper()

	_, err := db.New(e.pool).InsertUISession(t.Context(), db.InsertUISessionParams{
		IDHash: hashOf(cookieValue), Principal: "dave", Groups: append([]string{}, groups...),
		CsrfToken: hashOf("csrf-" + cookieValue),
	})
	require.NoError(t, err)

	browser := e.browser(t)

	serverURL, err := url.Parse(e.server.URL)
	require.NoError(t, err)
	browser.Jar.SetCookies(serverURL, []*http.Cookie{{Name: ui.SessionCookie, Value: cookieValue, Path: "/", Secure: true}})

	return browser
}

// csrfTokenOf is the CSRF token forms carry for the session insertSession
// stored under cookieValue.
func csrfTokenOf(cookieValue string) string {
	return ui.Session{CSRFToken: hashOf("csrf-" + cookieValue)}.CSRFTokenValue()
}

func hashOf(value string) []byte {
	sum := sha256.Sum256([]byte(value))

	return sum[:]
}

// [design row 64] A session idle for 1 h or older than 8 h no longer works:
// a GET redirects to sign-in and a POST gets 401.
func TestEndedSessionsAreRefused(t *testing.T) {
	env := startUIEnv(t, "admins")

	tests := []struct {
		name   string
		update string
	}{
		{name: "idle for an hour", update: "last_seen_at = now() - interval '1 hour'"},
		{name: "older than 8 hours", update: "created_at = now() - interval '9 hours', expires_at = now() - interval '1 hour'"},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cookie := fmt.Sprintf("ended-session-%d", index)
			browser := env.insertSession(t, cookie, []string{"admins"})

			response := get(t, browser, env.server.URL+"/ui/admin-only")
			require.Equal(t, http.StatusOK, response.StatusCode, "valid before it ends")

			_, err := env.pool.Exec(t.Context(), "UPDATE ui_sessions SET "+test.update+" WHERE id_hash = $1", hashOf(cookie))
			require.NoError(t, err)

			response = get(t, browser, env.server.URL+"/ui/admin-only")
			assert.Equal(t, http.StatusSeeOther, response.StatusCode)
			assert.Equal(t, ui.LoginPath+"?return_to=%2Fui%2Fadmin-only", response.Header.Get("Location"))

			response = post(t, browser, env.server.URL+"/ui/admin-only", url.Values{})
			assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		})
	}
}

func TestSessionLastSeenMovesAtMostOnceAMinute(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "touch-session", nil)

	lastSeenAge := func() time.Duration {
		var seconds float64
		require.NoError(t, env.pool.QueryRow(t.Context(),
			"SELECT extract(epoch FROM now() - last_seen_at) FROM ui_sessions WHERE id_hash = $1", hashOf("touch-session")).Scan(&seconds))

		return time.Duration(seconds * float64(time.Second))
	}

	setAge := func(age string) {
		_, err := env.pool.Exec(t.Context(), "UPDATE ui_sessions SET last_seen_at = now() - $2::interval WHERE id_hash = $1", hashOf("touch-session"), age)
		require.NoError(t, err)
	}

	setAge("30 seconds")
	env.whoami(t, browser)
	assert.GreaterOrEqual(t, lastSeenAge(), 29*time.Second, "not moved within a minute")

	setAge("2 minutes")
	env.whoami(t, browser)
	assert.Less(t, lastSeenAge(), 30*time.Second, "moved after a minute")
}

func TestAdminRoutes(t *testing.T) {
	tests := []struct {
		name       string
		adminGroup string
		groups     []string
		wantStatus int
	}{
		{name: "member of the admin group", adminGroup: "admins", groups: []string{"dev", "admins"}, wantStatus: http.StatusOK},
		{name: "not a member", adminGroup: "admins", groups: []string{"dev"}, wantStatus: http.StatusForbidden},
		{name: "empty admin group makes nobody an admin", adminGroup: "", groups: []string{""}, wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := startUIEnv(t, test.adminGroup)
			browser := env.insertSession(t, "admin-session", test.groups)

			response := get(t, browser, env.server.URL+"/ui/admin-only")
			assert.Equal(t, test.wantStatus, response.StatusCode)

			response = post(t, browser, env.server.URL+"/ui/admin-only", url.Values{ui.CSRFField: {csrfTokenOf("admin-session")}})
			assert.Equal(t, test.wantStatus, response.StatusCode)

			response = get(t, browser, env.server.URL+whoamiPath)
			assert.Equal(t, http.StatusOK, response.StatusCode, "user routes work for everyone signed in")
		})
	}
}

func TestLogoutDeletesSession(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "logout-session", nil)

	response := get(t, browser, env.server.URL+"/")
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.HomePath, response.Header.Get("Location"))

	response = post(t, browser, env.server.URL+ui.LogoutPath, url.Values{ui.CSRFField: {csrfTokenOf("logout-session")}})
	require.Equal(t, http.StatusOK, response.StatusCode)
	_, _ = io.Copy(io.Discard, response.Body)

	cleared := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cleared)
	assert.Negative(t, cleared.MaxAge)
	assert.Equal(t, 0, env.countRows(t, "ui_sessions"))

	// Even a browser that kept the cookie is signed out.
	kept := env.browser(t)
	serverURL, err := url.Parse(env.server.URL)
	require.NoError(t, err)
	kept.Jar.SetCookies(serverURL, []*http.Cookie{{Name: ui.SessionCookie, Value: "logout-session", Path: "/", Secure: true}})

	response = get(t, kept, env.server.URL+whoamiPath)
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)
}

func TestSignInWithAnotherNonceIsRefused(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	authURL, _ := env.startLogin(t, browser, "/ui/whoami")
	callback := env.signInAtIdP(t, browser, authURL, toolgatetest.OIDCUser{Subject: "erin"})

	// The ID token carries the nonce of the request; the stored one differs.
	_, err := env.pool.Exec(t.Context(), "UPDATE ui_logins SET nonce = 'another-nonce'")
	require.NoError(t, err)

	response := get(t, browser, callback.String())
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Nil(t, setCookie(response, ui.SessionCookie))
	assert.Equal(t, []string{"web UI sign-in: the ID token's nonce does not match"}, env.refusals.reasons())
}
