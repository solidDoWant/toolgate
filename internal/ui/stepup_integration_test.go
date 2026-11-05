//go:build integration

package ui_test

import (
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

const stepUpFormPath = "/ui/stepup-form"

// registerStepUpForm registers a form that needs a step-up, as the grant
// and approval pages do: its GET shows the form or the step-up button, and
// its POST counts the submits that ran.
func registerStepUpForm(webUI *ui.UI, submits *atomic.Int32) {
	webUI.Handle("GET "+stepUpFormPath, ui.User, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		session, _ := ui.SessionFrom(request.Context())
		if ui.StepUpNeeded(session) {
			ui.WriteStepUpPrompt(writer, request)

			return
		}

		_, _ = io.WriteString(writer, `<form method="post">the form</form>`)
	}))
	webUI.Handle("POST "+stepUpFormPath, ui.User, ui.RequireStepUp(
		func(*http.Request) string { return stepUpFormPath },
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { submits.Add(1) }),
	))
}

var alice = toolgatetest.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"dev"}}

// signIn completes a sign-in through loginPath and returns the callback's
// response.
func (e *uiEnv) signIn(t *testing.T, browser *http.Client, loginPath string, user toolgatetest.OIDCUser) *http.Response {
	t.Helper()

	response := get(t, browser, e.server.URL+loginPath)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	callback := e.signInAtIdP(t, browser, location(t, response), user)

	return get(t, browser, callback.String())
}

func (e *uiEnv) sessionRow(t *testing.T, cookieValue string) db.UiSession {
	t.Helper()

	row, err := db.New(e.pool).GetValidUISession(t.Context(), hashOf(cookieValue))
	require.NoError(t, err)

	return row
}

func (e *uiEnv) ageSignIn(t *testing.T, cookieValue string, age time.Duration) {
	t.Helper()

	_, err := e.pool.Exec(t.Context(), "UPDATE ui_sessions SET auth_time = now() - $2::interval WHERE id_hash = $1",
		hashOf(cookieValue), pgtype.Interval{Microseconds: age.Microseconds(), Valid: true})
	require.NoError(t, err)
}

func body(t *testing.T, response *http.Response) string {
	t.Helper()

	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return string(content)
}

func TestStepUpRedirectAsksForAFreshSignIn(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	normal, _ := env.startLogin(t, browser, "/ui/whoami")
	assert.Empty(t, normal.Query().Get("prompt"))
	assert.Empty(t, normal.Query().Get("max_age"))

	response := get(t, browser, env.server.URL+ui.StepUpURL(stepUpFormPath))
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	stepUp := location(t, response)
	assert.Equal(t, "login", stepUp.Query().Get("prompt"))
	assert.Equal(t, "0", stepUp.Query().Get("max_age"))
}

func TestStepUpUpdatesTheSession(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	response := env.signIn(t, browser, ui.LoginPath+"?return_to=/ui/whoami", alice)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	cookie := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cookie)
	env.ageSignIn(t, cookie.Value, 10*time.Minute)

	before := env.sessionRow(t, cookie.Value)

	changed := alice
	changed.Groups = []string{"dev", "ops"}

	response = env.signIn(t, browser, ui.StepUpURL(stepUpFormPath), changed)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, stepUpFormPath, location(t, response).Path)
	assert.Nil(t, setCookie(response, ui.SessionCookie), "the step-up keeps the session")

	after := env.sessionRow(t, cookie.Value)
	assert.Equal(t, []string{"dev", "ops"}, after.Groups, "the identity snapshot is updated")
	assert.WithinDuration(t, time.Now(), after.AuthTime.Time, time.Minute)
	assert.True(t, after.AuthTime.Time.After(before.AuthTime.Time))
	assert.Equal(t, 1, env.countRows(t, "ui_sessions"))
	assert.Empty(t, env.refusals.reasons())
}

func TestStepUpAsADifferentUserSignsOut(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	response := env.signIn(t, browser, ui.LoginPath, alice)
	cookie := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cookie)

	response = env.signIn(t, browser, ui.StepUpURL(stepUpFormPath), toolgatetest.OIDCUser{Subject: "mallory"})
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Contains(t, body(t, response), "You signed in as a different user.")

	cleared := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cleared)
	assert.Negative(t, cleared.MaxAge)
	assert.Equal(t, 0, env.countRows(t, "ui_sessions"), "the session is signed out")
	assert.Len(t, env.refusals.reasons(), 1)
}

func TestStepUpWithoutASessionCreatesOne(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	response := env.signIn(t, browser, ui.StepUpURL(stepUpFormPath), alice)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, stepUpFormPath, location(t, response).Path)

	cookie := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cookie)

	row := env.sessionRow(t, cookie.Value)
	assert.Equal(t, "alice", row.Principal)
	assert.WithinDuration(t, time.Now(), row.AuthTime.Time, time.Minute)
}

func TestStepUpRefusesAnAuthTimeBeforeTheSignInStarted(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	response := get(t, browser, env.server.URL+ui.StepUpURL(stepUpFormPath))
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	callback := env.signInAtIdP(t, browser, location(t, response), alice)

	// As if the IdP returned a sign-in from before the step-up started.
	_, err := env.pool.Exec(t.Context(), "UPDATE ui_logins SET created_at = now() + interval '2 minutes'")
	require.NoError(t, err)

	response = get(t, browser, callback.String())
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Nil(t, setCookie(response, ui.SessionCookie))
	assert.Equal(t, 0, env.countRows(t, "ui_sessions"))
	assert.Len(t, env.refusals.reasons(), 1)
}

func TestStepUpFailsClosedWithoutAuthTime(t *testing.T) {
	env := startUIEnvWithIdP(t, "", toolgatetest.OIDCOptions{OmitAuthTime: true})
	browser := env.browser(t)

	// A normal sign-in works without auth_time.
	response := env.signIn(t, browser, ui.LoginPath+"?return_to=/ui/whoami", alice)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)

	cookie := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cookie)

	response = env.signIn(t, browser, ui.StepUpURL(stepUpFormPath), alice)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Contains(t, body(t, response),
		"Your identity provider didn't say when you signed in, so toolgate can't confirm a fresh sign-in. "+
			"Ask an admin to make it send `auth_time`.")
	assert.False(t, env.sessionRow(t, cookie.Value).AuthTime.Valid, "the session is not marked fresh")

	// The rest of the UI keeps working, but the form asks for a step-up.
	identity := env.whoami(t, browser)
	assert.Equal(t, "alice", identity["principal"])

	response = get(t, browser, env.server.URL+stepUpFormPath)
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Contains(t, body(t, response), "Sign in again to continue")
}

// [design row 63] The form page asks for a step-up past 4 minutes, and a
// POST past 5 minutes changes nothing and redirects to a step-up that
// returns to the form.
func TestStepUpFormAndSubmit(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)

	response := env.signIn(t, browser, ui.LoginPath, alice)
	cookie := setCookie(response, ui.SessionCookie)
	require.NotNil(t, cookie)

	formPage := func() string {
		response := get(t, browser, env.server.URL+stepUpFormPath)
		require.Equal(t, http.StatusOK, response.StatusCode)

		return body(t, response)
	}

	assert.Contains(t, formPage(), "the form")

	response = post(t, browser, env.server.URL+stepUpFormPath, url.Values{})
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, int32(1), env.formSubmits.Load())

	env.ageSignIn(t, cookie.Value, 4*time.Minute+30*time.Second)

	page := formPage()
	assert.Contains(t, page, "Sign in again to continue")
	assert.NotContains(t, page, "the form")

	// Between 4 and 5 minutes, a submit still works.
	response = post(t, browser, env.server.URL+stepUpFormPath, url.Values{})
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, int32(2), env.formSubmits.Load())

	env.ageSignIn(t, cookie.Value, 5*time.Minute+30*time.Second)

	response = post(t, browser, env.server.URL+stepUpFormPath, url.Values{})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, int32(2), env.formSubmits.Load(), "the stale submit changes nothing")

	stepUp := location(t, response)
	assert.Equal(t, ui.LoginPath, stepUp.Path)
	assert.Equal(t, "1", stepUp.Query().Get("step_up"))
	assert.Equal(t, stepUpFormPath, stepUp.Query().Get("return_to"))

	// The step-up returns to the form, which shows again.
	response = env.signIn(t, browser, stepUp.RequestURI(), alice)
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, stepUpFormPath, location(t, response).Path)
	assert.Contains(t, formPage(), "the form")
}
