//go:build integration

package gateway_test

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// grantsUI is the web UI's Grants pages over the fixture's database and
// catalog, with a browser signed in as one principal a moment ago.
type grantsUI struct {
	url     string
	browser *http.Client
	token   string
}

type uiMux struct{ mux *http.ServeMux }

func (m uiMux) HandlePublic(pattern string, handler http.Handler) { m.mux.Handle(pattern, handler) }
func (m uiMux) Handle(pattern string, handler http.Handler)       { m.mux.Handle(pattern, handler) }

func (f *mcpFixture) startGrantsUI(t *testing.T, principal string, groups []string) grantsUI {
	t.Helper()

	mux := uiMux{mux: http.NewServeMux()}
	server := httptest.NewTLSServer(mux.mux)
	t.Cleanup(server.Close)

	ui.New(ui.Options{
		Database:  f.pool,
		IssuerURL: "https://idp.invalid",
		ClientID:  "toolgate-ui",
		PublicURL: server.URL,
		Grants:    gateway.NewGrants(f.pool, f.catalog, io.Discard),
	}, mux)

	cookie := "grants-ui-" + principal
	idHash := sha256.Sum256([]byte(cookie))
	csrf := sha256.Sum256([]byte("csrf-" + cookie))

	_, err := db.New(f.pool).InsertUISession(t.Context(), db.InsertUISessionParams{
		IDHash: idHash[:], Principal: principal, Groups: append([]string{}, groups...), CsrfToken: csrf[:],
		AuthTime: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	require.NoError(t, err)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	jar.SetCookies(serverURL, []*http.Cookie{{Name: ui.SessionCookie, Value: cookie, Path: "/", Secure: true}})

	browser := server.Client()
	browser.Jar = jar
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return grantsUI{url: server.URL, browser: browser, token: base64.RawURLEncoding.EncodeToString(csrf[:])}
}

func (g grantsUI) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()

	form.Set(ui.CSRFField, g.token)

	response, err := g.browser.PostForm(g.url+path, form)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

// create submits the grant form and returns the new grant's ID.
func (g grantsUI) create(t *testing.T, f *mcpFixture, principal string, form url.Values) string {
	t.Helper()

	response := g.post(t, ui.GrantsPath, form)
	body, _ := io.ReadAll(response.Body)
	require.Equal(t, http.StatusSeeOther, response.StatusCode, string(body))

	grants, err := db.New(f.pool).ListGrantsOfPrincipal(t.Context(), db.ListGrantsOfPrincipalParams{Principal: principal, MaxRows: 10})
	require.NoError(t, err)
	require.NotEmpty(t, grants)

	return grants[0].ID
}

// A signed-in laptop user can't enable an opt-in server. After
// creating a grant for it on the Grants page they can. After the grant is
// revoked or expires, enabling is refused, and an already enabled server's
// tools are withdrawn within 5 s.
func TestGrantEnablesOptInServer(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", optInForAlice)
		fixture.startUpstream(t, "github")
		page := fixture.startGrantsUI(t, "alice", nil)

		client := fixture.connectObserved(t, alice())
		assert.Contains(t, awaitEnable(t, startEnable(t, client.session, "github")).err, "attested environment", "refused without a grant")

		grant := page.create(t, fixture, "alice", url.Values{"server": {"github"}, "duration": {"1h"}})
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond,
			"the grant makes the server usable, which changes the discovery tools' descriptions")

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)

		revoked := time.Now()
		response := page.post(t, "/ui/grants/"+grant+"/revoke", url.Values{})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, policyChangeBound, 10*time.Millisecond,
			"no list_changed within 5 s of the revoke")
		assert.Less(t, time.Since(revoked), policyChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session), "the server's tools are withdrawn")

		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, refused.IsError)

		other := alice()
		other.session = "session-2"
		assert.Contains(t, awaitEnable(t, startEnable(t, fixture.connectObserved(t, other).session, "github")).err,
			"attested environment", "enabling is refused after the revoke")
	})

	t.Run("expired", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", optInForAlice)
		fixture.startUpstream(t, "github")
		page := fixture.startGrantsUI(t, "alice", nil)

		client := fixture.connectObserved(t, alice())

		// The shortest grant the page offers.
		grant := page.create(t, fixture, "alice", url.Values{"server": {"github"}, "duration": {"1m"}})
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond,
			"the grant makes the server usable, which changes the discovery tools' descriptions")

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)

		row, err := db.New(fixture.pool).GetGrant(t.Context(), grant)
		require.NoError(t, err)

		expiresAt := row.ExpiresAt.Time
		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, time.Until(expiresAt)+policyChangeBound, 50*time.Millisecond,
			"no list_changed within 5 s of the grant's expiry")
		assert.Less(t, time.Since(expiresAt), policyChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session), "the server's tools are withdrawn")

		other := alice()
		other.session = "session-2"
		assert.Contains(t, awaitEnable(t, startEnable(t, fixture.connectObserved(t, other).session, "github")).err,
			"attested environment", "enabling is refused after the expiry")
	})
}

// A grant narrowed to one session on the Grants page lets that
// session enable the server and no other session of the same user.
func TestSessionNarrowedGrant(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", optInForAlice)
	fixture.startUpstream(t, "github")
	page := fixture.startGrantsUI(t, "alice", nil)

	narrowed := fixture.connectObserved(t, alice())
	otherSession := alice()
	otherSession.session = "session-2"
	other := fixture.connectObserved(t, otherSession)

	// Both sessions are open, so the form offers them.
	assert.Contains(t, awaitEnable(t, startEnable(t, narrowed.session, "github")).err, "attested environment")
	assert.Contains(t, awaitEnable(t, startEnable(t, other.session, "github")).err, "attested environment")

	response, err := page.browser.Get(page.url + ui.NewGrantPath + "?server=github&session=session-1")
	require.NoError(t, err)

	form, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	assert.Contains(t, string(form), `<option value="session-1" selected>session-1</option>`)
	assert.Contains(t, string(form), `<option value="session-2">session-2</option>`)

	page.create(t, fixture, "alice", url.Values{"server": {"github"}, "duration": {"1h"}, "session": {"session-1"}})

	assert.Contains(t, awaitEnable(t, startEnable(t, other.session, "github")).err, "attested environment",
		"another session of the same user is refused")
	fixture.enableWithTools(t, narrowed, "github", "user:alice", "echo")
}
