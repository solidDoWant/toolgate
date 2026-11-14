//go:build integration

package ui_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// browse opens target and follows redirects as a browser would. Each time
// the fake IdP shows its sign-in form, it signs in as user and counts it. It
// returns the body of the page it stops at and the number of sign-ins.
func (e *uiEnv) browse(t *testing.T, browser *http.Client, target string, user toolgatetest.OIDCUser) (string, int) {
	t.Helper()

	next, err := url.Parse(target)
	require.NoError(t, err)

	next = mustParse(t, e.server.URL).ResolveReference(next)
	signIns := 0

	for range 30 {
		response := get(t, browser, next.String())

		switch {
		case response.StatusCode == http.StatusOK && next.Path == toolgatetest.OIDCSignInPath:
			signIns++
			response = post(t, browser, next.String(), url.Values{
				"authRequestID":  {next.Query().Get("authRequestID")},
				"subject":        {user.Subject},
				"email":          {user.Email},
				"email_verified": {"true"},
				"groups":         {strings.Join(user.Groups, ",")},
			})
			require.Contains(t, []int{http.StatusFound, http.StatusSeeOther}, response.StatusCode)
			next = next.ResolveReference(location(t, response))
		case response.StatusCode == http.StatusFound || response.StatusCode == http.StatusSeeOther:
			next = next.ResolveReference(location(t, response))
		default:
			return readBody(t, response), signIns
		}
	}

	require.FailNow(t, "too many redirects")

	return "", 0
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	require.NoError(t, err)

	return parsed
}

var csrfInput = regexp.MustCompile(`name="` + ui.CSRFField + `" value="([^"]+)"`)

// csrfTokenOn returns the CSRF token of the first form on page.
func csrfTokenOn(t *testing.T, page string) string {
	t.Helper()

	match := csrfInput.FindStringSubmatch(page)
	require.NotNil(t, match, "the page has a form")

	return match[1]
}

// sessionCookieOf returns the value of the browser's UI session cookie.
func (e *uiEnv) sessionCookieOf(t *testing.T, browser *http.Client) string {
	t.Helper()

	for _, cookie := range browser.Jar.Cookies(mustParse(t, e.server.URL)) {
		if cookie.Name == ui.SessionCookie {
			return cookie.Value
		}
	}

	require.FailNow(t, "the browser has no UI session")

	return ""
}

// browserWithIdPSessionOnly signs a browser in once, so the fake IdP
// remembers it, and then ends every UI session.
func (e *uiEnv) browserWithIdPSessionOnly(t *testing.T, user toolgatetest.OIDCUser) *http.Client {
	t.Helper()

	browser := e.browser(t)
	_, signIns := e.browse(t, browser, ui.LoginPath+"?return_to="+whoamiPath, user)
	require.Equal(t, 1, signIns)

	_, err := e.pool.Exec(t.Context(), "DELETE FROM ui_sessions")
	require.NoError(t, err)

	return browser
}

// approve opens the approval page of id, approves it and returns the number
// of IdP sign-ins it took.
func (e *uiEnv) approve(t *testing.T, browser *http.Client, id string, user toolgatetest.OIDCUser) int {
	t.Helper()

	page, signIns := e.browse(t, browser, ui.ApprovalPath(id), user)
	if !strings.Contains(page, "/approve") {
		var more int

		page, more = e.browse(t, browser, ui.StepUpURL(ui.ApprovalPath(id)), user)
		signIns += more
	}

	response := post(t, browser, e.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOn(t, page)}})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	require.NotEqual(t, ui.LoginPath, location(t, response).Path, "the approval needs no other sign-in")
	require.Equal(t, "approved", e.approvalRow(t, id).Status)

	return signIns
}

var dave = toolgatetest.OIDCUser{Subject: "dave", Email: "dave@example.com", EmailVerified: true}

func TestStepUpPagesWithAnIdPSessionNeedOneSignIn(t *testing.T) {
	env := startUIEnv(t, "")

	t.Run("approval", func(t *testing.T) {
		browser := env.browserWithIdPSessionOnly(t, dave)
		id := approvalID(0)
		env.insertApproval(t, id, dave.Subject, approvalTimeout)

		assert.Equal(t, 1, env.approve(t, browser, id, dave))
	})

	t.Run("grant form", func(t *testing.T) {
		browser := env.browserWithIdPSessionOnly(t, dave)

		page, signIns := env.browse(t, browser, ui.NewGrantPath+"?server=github", dave)
		assert.Equal(t, 1, signIns)
		assert.NotContains(t, page, "Sign in again to continue")
	})

	t.Run("workspaces page with a pending change", func(t *testing.T) {
		browser := env.browserWithIdPSessionOnly(t, dave)

		target := ui.WorkspacesPath + "?" + url.Values{"pending": {"allow"}, "workspace": {"github.com/acme/repo"}, "server": {"github"}}.Encode()
		page, signIns := env.browse(t, browser, target, dave)
		assert.Equal(t, 1, signIns)
		assert.Contains(t, page, "github.com/acme/repo")
		assert.NotContains(t, page, "/ui/login?", "the page asks for no other sign-in")
	})

	t.Run("workspaces page without a pending change", func(t *testing.T) {
		browser := env.browserWithIdPSessionOnly(t, dave)

		_, signIns := env.browse(t, browser, ui.WorkspacesPath, dave)
		assert.Zero(t, signIns, "the IdP session signs the user in")
	})
}

func TestStepUpWindowCoversApprovalsWithinIt(t *testing.T) {
	tests := []struct {
		name        string
		maxAge      time.Duration
		wantSignIns int
	}{
		{name: "default", wantSignIns: 2},
		{name: "30 minutes", maxAge: 30 * time.Minute, wantSignIns: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := startUIEnvConfigured(t, "", toolgatetest.OIDCOptions{}, func(options *ui.Options) {
				options.StepUpMaxAge = test.maxAge
			})
			browser := env.browser(t)

			first, second := approvalID(0), approvalID(1)
			env.insertApproval(t, first, dave.Subject, time.Hour)
			env.insertApproval(t, second, dave.Subject, time.Hour)

			signIns := env.approve(t, browser, first, dave)

			// Ten minutes pass between the two approvals.
			env.ageSignIn(t, env.sessionCookieOf(t, browser), 10*time.Minute)

			signIns += env.approve(t, browser, second, dave)
			assert.Equal(t, test.wantSignIns, signIns)
		})
	}
}
