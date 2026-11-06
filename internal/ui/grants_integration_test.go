//go:build integration

package ui_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// addGrantTemplates offers an opt-in server and an any server with an
// opt-in tool to the group dev.
func (e *uiEnv) addGrantTemplates() {
	for name, spec := range map[string]v1alpha1.MCPServerTemplateSpec{
		"optin-server": {Scope: "per-user", Access: v1alpha1.Access{Groups: []string{"dev"}, Environments: "opt-in"}},
		"any-server": {
			Scope: "per-user", Access: v1alpha1.Access{Groups: []string{"dev"}, Environments: "any"},
			Tools: map[string]v1alpha1.ToolOverride{"deploy": {Environments: "opt-in"}},
		},
	} {
		e.catalog.Set(&v1alpha1.MCPServerTemplate{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tools", Name: name, Generation: 1, UID: types.UID("uid-" + name)},
			Spec:       spec,
		}, nil)
	}
}

// freshSignIn sets the session's sign-in time to now.
func (e *uiEnv) freshSignIn(t *testing.T, cookieValue string) {
	t.Helper()

	_, err := e.pool.Exec(t.Context(), "UPDATE ui_sessions SET auth_time = now() WHERE id_hash = $1", hashOf(cookieValue))
	require.NoError(t, err)
}

func (e *uiEnv) openSession(t *testing.T, principal, session string) {
	t.Helper()

	_, err := db.New(e.pool).CreateSession(t.Context(), db.CreateSessionParams{
		Principal: principal, SessionID: session, Workspace: "w", Attestation: auth.AttestationUser, Kind: auth.KindUser, Groups: []string{},
	})
	require.NoError(t, err)
}

func (e *uiEnv) grantsOf(t *testing.T, principal string) []db.Grant {
	t.Helper()

	grants, err := db.New(e.pool).ListGrantsOfPrincipal(t.Context(), db.ListGrantsOfPrincipalParams{Principal: principal, MaxRows: 1000})
	require.NoError(t, err)

	return grants
}

func (e *uiEnv) grantEventTypes(t *testing.T) []string {
	t.Helper()

	rows, err := e.pool.Query(t.Context(), "SELECT type FROM audit_events WHERE type LIKE 'grant.%' ORDER BY occurred_at, id")
	require.NoError(t, err)

	defer rows.Close()

	var types []string

	for rows.Next() {
		var eventType string
		require.NoError(t, rows.Scan(&eventType))
		types = append(types, eventType)
	}

	return types
}

func postWithHeader(t *testing.T, client *http.Client, target string, form url.Values, header http.Header) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for name, values := range header {
		request.Header[name] = values
	}

	response, err := client.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

func TestGrantFormIsFilledFromTheLink(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()
	env.openSession(t, "dave", "laptop-1")
	env.openSession(t, "frank", "franks-laptop")

	browser := env.insertSession(t, "grant-form", []string{"dev"})
	env.freshSignIn(t, "grant-form")

	link := ui.GrantLink(env.server.URL+"/")("any-server", "deploy", "laptop-1")
	assert.Equal(t, env.server.URL+"/ui/grants/new?server=any-server&session=laptop-1&tool=deploy", link)

	response := get(t, browser, link)
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)
	assert.Contains(t, page, `<option value="any-server" selected>any-server</option>`)
	assert.Contains(t, page, `<option value="optin-server">optin-server</option>`)
	assert.Contains(t, page, `<option value="deploy" selected>any-server__deploy</option>`)
	assert.Contains(t, page, `<option value="laptop-1" selected>laptop-1</option>`)
	assert.NotContains(t, page, "franks-laptop", "only the user's own sessions are offered")
	assert.Contains(t, page, `<input type="hidden" name="csrf_token" value="`+csrfTokenOf("grant-form")+`">`)
}

func TestGrantCreateListAndRevoke(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()
	env.openSession(t, "dave", "laptop-1")

	browser := env.insertSession(t, "grant-create", []string{"dev"})
	env.freshSignIn(t, "grant-create")

	token := csrfTokenOf("grant-create")

	response := post(t, browser, env.server.URL+ui.GrantsPath, url.Values{
		ui.CSRFField: {token}, "server": {"optin-server"}, "duration": {"1h"}, "session": {"laptop-1"},
	})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.GrantsPath, location(t, response).Path)

	grants := env.grantsOf(t, "dave")
	require.Len(t, grants, 1)
	assert.Equal(t, "optin-server", grants[0].TemplateName)
	assert.Equal(t, pgtype.Text{String: "laptop-1", Valid: true}, grants[0].SessionID)

	// A refused grant shows the form again with the reason, and adds no row.
	response = post(t, browser, env.server.URL+ui.GrantsPath, url.Values{
		ui.CSRFField: {token}, "server": {"optin-server"}, "tool": {"deploy"}, "duration": {"1h"},
	})
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	assert.Contains(t, readBody(t, response), "can&#39;t have its own grant")

	response = post(t, browser, env.server.URL+ui.GrantsPath, url.Values{
		ui.CSRFField: {token}, "server": {"optin-server"}, "duration": {"8d"},
	})
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	assert.Len(t, env.grantsOf(t, "dave"), 1)

	response = post(t, browser, env.server.URL+ui.GrantsPath, url.Values{
		ui.CSRFField: {token}, "server": {"any-server"}, "tool": {"deploy"}, "duration": {"1m"},
	})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	grants = env.grantsOf(t, "dave")
	require.Len(t, grants, 2)

	toolGrant, serverGrant := grants[0], grants[1]

	response = get(t, browser, env.server.URL+ui.GrantsPath)
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)
	assert.Contains(t, page, `<tr id="grant-`+serverGrant.ID+`">`)
	assert.Contains(t, page, `<td>deploy</td>`)
	assert.Contains(t, page, `hx-post="/ui/grants/`+toolGrant.ID+`/revoke" hx-target="closest tr" hx-swap="outerHTML"`)

	// htmx gets the revoked row to swap in.
	response = postWithHeader(t, browser, env.server.URL+"/ui/grants/"+serverGrant.ID+"/revoke", url.Values{},
		http.Header{"Hx-Request": {"true"}, ui.CSRFHeader: {token}})
	require.Equal(t, http.StatusOK, response.StatusCode)

	row := readBody(t, response)
	assert.True(t, strings.HasPrefix(row, `<tr id="grant-`+serverGrant.ID+`">`), row)
	assert.Contains(t, row, "revoked")
	assert.NotContains(t, row, "<form", "a revoked grant has no Revoke button")

	// A plain form is redirected to the list.
	response = post(t, browser, env.server.URL+"/ui/grants/"+toolGrant.ID+"/revoke", url.Values{ui.CSRFField: {token}})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.GrantsPath, location(t, response).Path)

	for _, grant := range env.grantsOf(t, "dave") {
		assert.True(t, grant.RevokedAt.Valid, "grant %s is revoked", grant.ID)
	}

	assert.Equal(t, []string{"grant.create", "grant.create", "grant.revoke", "grant.revoke"}, env.grantEventTypes(t))
}

func TestGrantRevokeNeedsNoStepUp(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()

	grant, err := store.InsertGrant(t.Context(), env.pool, db.InsertGrantWithinLimitParams{
		ID: "0123456789abcdef0123456789abcdef", Principal: "dave", TemplateNs: "tools", TemplateName: "optin-server",
		Lifetime: pgtype.Interval{Microseconds: time.Hour.Microseconds(), Valid: true},
	})
	require.NoError(t, err)

	// insertSession stores no sign-in time, so every step-up check fails.
	browser := env.insertSession(t, "grant-stale-revoke", []string{"dev"})

	response := post(t, browser, env.server.URL+"/ui/grants/"+grant.ID+"/revoke", url.Values{ui.CSRFField: {csrfTokenOf("grant-stale-revoke")}})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.GrantsPath, location(t, response).Path)
	assert.True(t, env.grantsOf(t, "dave")[0].RevokedAt.Valid)
}

func TestGrantCreateNeedsAFreshSignIn(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()

	browser := env.insertSession(t, "grant-stale", []string{"dev"})
	env.freshSignIn(t, "grant-stale")
	env.ageSignIn(t, "grant-stale", 5*time.Minute+30*time.Second)

	response := get(t, browser, env.server.URL+ui.NewGrantPath+"?server=optin-server")
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)
	assert.Contains(t, page, "Sign in again to continue")
	assert.NotContains(t, page, "grant-form")

	response = post(t, browser, env.server.URL+ui.GrantsPath, url.Values{
		ui.CSRFField: {csrfTokenOf("grant-stale")}, "server": {"optin-server"}, "duration": {"1h"},
	})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	stepUp := location(t, response)
	assert.Equal(t, ui.LoginPath, stepUp.Path)
	assert.Equal(t, "1", stepUp.Query().Get("step_up"))
	assert.Equal(t, ui.NewGrantPath+"?server=optin-server", stepUp.Query().Get("return_to"))
	assert.Empty(t, env.grantsOf(t, "dave"), "the stale submit creates nothing")
}

func TestAnotherUsersGrantIsNotFound(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.addGrantTemplates()

	grant, err := store.InsertGrant(t.Context(), env.pool, db.InsertGrantWithinLimitParams{
		ID: "fedcba9876543210fedcba9876543210", Principal: "frank", TemplateNs: "tools", TemplateName: "optin-server",
		Lifetime: pgtype.Interval{Microseconds: time.Hour.Microseconds(), Valid: true},
	})
	require.NoError(t, err)

	for _, test := range []struct {
		name   string
		groups []string
	}{
		{name: "user", groups: []string{"dev"}},
		{name: "admin", groups: []string{"dev", "admins"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cookie := "other-grant-" + test.name
			browser := env.insertSession(t, cookie, test.groups)
			env.freshSignIn(t, cookie)

			response := post(t, browser, env.server.URL+"/ui/grants/"+grant.ID+"/revoke", url.Values{ui.CSRFField: {csrfTokenOf(cookie)}})
			assert.Equal(t, http.StatusNotFound, response.StatusCode)

			response = post(t, browser, env.server.URL+"/ui/grants/"+strings.Repeat("0", 32)+"/revoke", url.Values{ui.CSRFField: {csrfTokenOf(cookie)}})
			assert.Equal(t, http.StatusNotFound, response.StatusCode, "a missing grant looks the same")

			response = get(t, browser, env.server.URL+ui.GrantsPath)
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.NotContains(t, readBody(t, response), grant.ID)
		})
	}

	assert.False(t, env.grantsOf(t, "frank")[0].RevokedAt.Valid)
}

func TestGrantPagesNeedASession(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	response := post(t, browser, env.server.URL+ui.GrantsPath, url.Values{"server": {"optin-server"}, "duration": {"1h"}})
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	response = post(t, browser, env.server.URL+"/ui/grants/"+strings.Repeat("0", 32)+"/revoke", url.Values{})
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	response = get(t, browser, env.server.URL+ui.GrantsPath)
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)
}

// [AC-2-09] In a browser, creating a grant when the sign-in is older than 5
// minutes sends the user to the IdP first, and creates the grant only after
// the fresh sign-in.
func TestAC2_09GrantNeedsAFreshSignIn(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 3*time.Minute)
	defer cancel()

	idpTitle := "Sign in to the fake identity provider"
	user := toolgatetest.OIDCUser{Subject: "grace", Email: "grace@example.com", EmailVerified: true, Groups: []string{"dev"}}
	createButton := chromedp.CSS(`#grant-form button[type="submit"]`)

	require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+ui.NewGrantPath+"?server=optin-server")))
	require.NoError(t, chromedp.Do(ctx, toolgatetest.SignInToOIDC(user), chromedp.WaitVisible(createButton)))

	_, err := env.pool.Exec(t.Context(), "UPDATE ui_sessions SET auth_time = now() - interval '6 minutes' WHERE principal = $1", user.Subject)
	require.NoError(t, err)

	require.NoError(t, chromedp.Do(ctx, chromedp.Click(createButton)))

	require.Eventually(t, func() bool {
		title, err := chromedp.Run(ctx, chromedp.Title())

		return err == nil && title == idpTitle
	}, time.Minute, 100*time.Millisecond, "the browser is sent to the IdP")
	assert.Empty(t, env.grantsOf(t, user.Subject), "no grant before the fresh sign-in")

	// The fresh sign-in returns to the filled form, which now creates the
	// grant.
	require.NoError(t, chromedp.Do(ctx, toolgatetest.SignInToOIDC(user), chromedp.WaitVisible(createButton)))

	server, err := chromedp.Run(ctx, chromedp.Value(chromedp.CSS(`#grant-form select[name="server"]`)))
	require.NoError(t, err)
	assert.Equal(t, "optin-server", server)

	require.NoError(t, chromedp.Do(ctx, chromedp.Click(createButton)))

	assert.Eventually(t, func() bool { return len(env.grantsOf(t, user.Subject)) == 1 }, time.Minute, 100*time.Millisecond)
	assert.Eventually(t, func() bool { return pageShows(ctx, "optin-server") && pageShows(ctx, "Revoke") },
		time.Minute, 100*time.Millisecond, "the browser shows the grant")
}
