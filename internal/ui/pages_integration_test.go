//go:build integration

package ui_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// fakeServers is the Servers page's source. It records what it was asked.
type fakeServers struct {
	mu            sync.Mutex
	identities    []auth.Identity
	includeShared []bool
	restarts      []restartCall
}

// restartCall is one RequestRestart the fake was asked for.
type restartCall struct {
	id, principal string
	admin         bool
}

func (f *fakeServers) Servers(_ context.Context, identity auth.Identity) ([]gateway.ServerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.identities = append(f.identities, identity)

	return []gateway.ServerEntry{
		{Name: "github", Description: "The GitHub server.", Scope: "per-user", Status: "ready"},
		{Name: "prod-db", Scope: "shared", Status: "blocked", Reason: "Only from an attested environment."},
	}, nil
}

func (f *fakeServers) LiveInstances(_ context.Context, principal string, includeShared bool) ([]gateway.InstanceEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.includeShared = append(f.includeShared, includeShared)

	instances := []gateway.InstanceEntry{{ID: "own", Server: "github", Scope: "per-user", Owner: principal, Status: "ready", Outdated: true}}
	if includeShared {
		instances = append(instances, gateway.InstanceEntry{ID: "shared", Server: "wiki", Scope: "shared", Status: "starting", Outdated: true})
	}

	return instances, nil
}

// RequestRestart answers by the instance ID: "gone" isn't found,
// "changed" has changed, and any other ID is restarted.
func (f *fakeServers) RequestRestart(_ context.Context, id, principal string, admin bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.restarts = append(f.restarts, restartCall{id: id, principal: principal, admin: admin})

	switch id {
	case "gone":
		return gateway.ErrInstanceNotFound
	case "changed":
		return gateway.ErrInstanceChanged
	default:
		return nil
	}
}

func (f *fakeServers) restartCalls() []restartCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]restartCall{}, f.restarts...)
}

func (f *fakeServers) last() (auth.Identity, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.identities[len(f.identities)-1], f.includeShared[len(f.includeShared)-1]
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return string(body)
}

// probe registers a user POST route that counts the requests it handled.
func (e *uiEnv) probe(path string) *atomic.Int32 {
	var handled atomic.Int32

	e.replicas[e.target.Load()].ui.Handle("POST "+path, ui.User, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		handled.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))

	return &handled
}

func TestPostsNeedTheSessionCSRFToken(t *testing.T) {
	env := startUIEnv(t, "")
	handled := env.probe("/ui/probe")
	browser := env.insertSession(t, "csrf-session", nil)
	token := csrfTokenOf("csrf-session")

	send := func(t *testing.T, body string, header http.Header) *http.Response {
		t.Helper()

		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.server.URL+"/ui/probe", strings.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		for name, values := range header {
			request.Header[name] = values
		}

		response, err := browser.Do(request)
		require.NoError(t, err)
		t.Cleanup(func() { _ = response.Body.Close() })

		return response
	}

	tests := []struct {
		name       string
		body       string
		header     http.Header
		wantStatus int
	}{
		{name: "no token", wantStatus: http.StatusForbidden},
		{name: "wrong field", body: url.Values{ui.CSRFField: {csrfTokenOf("other-session")}}.Encode(), wantStatus: http.StatusForbidden},
		{name: "truncated field", body: url.Values{ui.CSRFField: {token[:len(token)-1]}}.Encode(), wantStatus: http.StatusForbidden},
		{name: "wrong header", header: http.Header{ui.CSRFHeader: {"forged"}}, wantStatus: http.StatusForbidden},
		{
			name: "wrong header wins over a right field", body: url.Values{ui.CSRFField: {token}}.Encode(),
			header: http.Header{ui.CSRFHeader: {"forged"}}, wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-site request with the right token", body: url.Values{ui.CSRFField: {token}}.Encode(),
			header: http.Header{"Sec-Fetch-Site": {"cross-site"}}, wantStatus: http.StatusForbidden,
		},
		{
			name: "other origin with the right token", body: url.Values{ui.CSRFField: {token}}.Encode(),
			header: http.Header{"Origin": {"https://attacker.example"}}, wantStatus: http.StatusForbidden,
		},
		{name: "right field", body: url.Values{ui.CSRFField: {token}}.Encode(), wantStatus: http.StatusNoContent},
		{name: "right header", header: http.Header{ui.CSRFHeader: {token}}, wantStatus: http.StatusNoContent},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := handled.Load()
			response := send(t, test.body, test.header)
			assert.Equal(t, test.wantStatus, response.StatusCode)

			if test.wantStatus == http.StatusForbidden {
				assert.Equal(t, before, handled.Load(), "the handler didn't run")
			}
		})
	}
}

func TestLogoutWithoutTokenKeepsSession(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "logout-csrf", nil)

	response := post(t, browser, env.server.URL+ui.LogoutPath, url.Values{ui.CSRFField: {"forged"}})
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Nil(t, setCookie(response, ui.SessionCookie), "the cookie is kept")
	assert.Equal(t, 1, env.countRows(t, "ui_sessions"), "the session is kept")
}

func TestFormBodiesAreLimited(t *testing.T) {
	env := startUIEnv(t, "")
	handled := env.probe("/ui/probe")
	browser := env.insertSession(t, "limit-session", nil)

	prefix := url.Values{ui.CSRFField: {csrfTokenOf("limit-session")}}.Encode() + "&pad="

	tests := []struct {
		name       string
		size       int
		wantStatus int
	}{
		{name: "exactly the limit", size: ui.MaxFormBytes, wantStatus: http.StatusNoContent},
		{name: "one byte over", size: ui.MaxFormBytes + 1, wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := handled.Load()
			body := prefix + strings.Repeat("a", test.size-len(prefix))

			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.server.URL+"/ui/probe", strings.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			response, err := browser.Do(request)
			require.NoError(t, err)
			t.Cleanup(func() { _ = response.Body.Close() })

			assert.Equal(t, test.wantStatus, response.StatusCode)

			if test.wantStatus != http.StatusNoContent {
				assert.Equal(t, before, handled.Load(), "the handler didn't run")
			}
		})
	}
}

func TestServersPage(t *testing.T) {
	tests := []struct {
		name       string
		groups     []string
		wantShared bool
	}{
		{name: "user", groups: []string{"dev"}},
		{name: "admin", groups: []string{"dev", "admins"}, wantShared: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := startUIEnv(t, "admins")
			browser := env.insertSession(t, "servers-session", test.groups)

			response := get(t, browser, env.server.URL+ui.HomePath)
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
			assert.Equal(t, "default-src 'self'; frame-ancestors 'none'", response.Header.Get("Content-Security-Policy"))
			assert.Equal(t, "text/html; charset=utf-8", response.Header.Get("Content-Type"))

			body := readBody(t, response)

			identity, includeShared := env.servers.last()
			assert.Equal(t, auth.Identity{Principal: "dave", Groups: test.groups, Attestation: auth.AttestationUser}, identity,
				"the catalog is the user's with `user` attestation and no workspace or session claim")
			assert.Equal(t, test.wantShared, includeShared)

			assert.Contains(t, body, "<td>github</td>")
			assert.Contains(t, body, "Only from an attested environment.")
			assert.Contains(t, body, "<td>outdated</td>")
			assert.Equal(t, test.wantShared, strings.Contains(body, "<td>wiki</td>"), "shared instances only for admins")
			assert.Equal(t, test.wantShared, strings.Contains(body, `href="/ui/blocks"`), "the Blocks link only for admins")

			token := csrfTokenOf("servers-session")
			assert.Contains(t, body, `<body hx-headers='{"X-CSRF-Token": "`+token+`"}'>`)
			assert.Contains(t, body, `<input type="hidden" name="csrf_token" value="`+token+`">`)
			assert.Contains(t, body, `<script src="/ui/static/htmx-2.0.7.min.js" defer></script>`)
			assert.NotContains(t, body, "<script>", "no inline script")
		})
	}
}

func TestRestartInstance(t *testing.T) {
	env := startUIEnv(t, "admins")
	browser := env.insertSession(t, "restart-session", []string{"admins"})
	form := url.Values{ui.CSRFField: {csrfTokenOf("restart-session")}}

	t.Run("the Servers page shows a restart button on an outdated ready instance only", func(t *testing.T) {
		body := readBody(t, get(t, browser, env.server.URL+ui.HomePath))

		assert.Contains(t, body, `<form method="post" action="/ui/instances/own/restart">`)
		assert.NotContains(t, body, `/ui/instances/shared/restart`, "the shared instance is starting")
	})

	t.Run("a restart request redirects to the Servers page, which says it was requested", func(t *testing.T) {
		response := post(t, browser, env.server.URL+"/ui/instances/own/restart", form)
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, restartCall{id: "own", principal: "dave", admin: true}, env.servers.restartCalls()[0])

		body := readBody(t, get(t, browser, location(t, response).String()))
		assert.Contains(t, body, "The restart was requested.")
	})

	t.Run("an instance that changed sends the user back to the Servers page, which says so", func(t *testing.T) {
		response := post(t, browser, env.server.URL+"/ui/instances/changed/restart", form)
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		target := location(t, response)
		assert.Equal(t, ui.HomePath, target.Path)

		body := readBody(t, get(t, browser, target.String()))
		assert.Contains(t, body, "The instance changed before the restart request arrived.")
	})

	t.Run("an instance that isn't the user's is not found", func(t *testing.T) {
		response := post(t, browser, env.server.URL+"/ui/instances/gone/restart", form)
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
	})

	t.Run("without a session the request is refused", func(t *testing.T) {
		before := len(env.servers.restartCalls())
		response := post(t, env.browser(t), env.server.URL+"/ui/instances/own/restart", form)
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Len(t, env.servers.restartCalls(), before)
	})
}

// pageShows reports whether the browser's page text contains text. It is
// false while the page is still loading.
func pageShows(ctx context.Context, text string) bool {
	body, err := chromedp.Run(ctx, chromedp.Text(chromedp.CSS("body")))

	return err == nil && strings.Contains(body, text)
}

// [AC-2-03] In a browser, a form submitted without the session's CSRF token
// is refused and changes nothing.
func TestAC2_03FormWithoutCSRFTokenIsRefused(t *testing.T) {
	env := startUIEnv(t, "")
	env.insertSession(t, "browser-csrf", nil)

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 2*time.Minute)
	defer cancel()

	secure := true
	_, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{
		Name: ui.SessionCookie, Value: "browser-csrf", URL: env.server.URL + "/", Path: "/", Secure: &secure, HTTPOnly: &secure,
	})
	require.NoError(t, err)

	logout := chromedp.CSS(`form.logout button`)
	openServers := chromedp.Steps(chromedp.Navigate(env.server.URL+ui.HomePath), chromedp.WaitVisible(logout))

	require.NoError(t, chromedp.Do(ctx, openServers))

	title, err := chromedp.Run(ctx, chromedp.Title())
	require.NoError(t, err)
	require.Equal(t, "Servers · toolgate", title, "signed in")

	require.NoError(t, chromedp.Do(ctx,
		chromedp.SetValue(chromedp.CSS(`form.logout input[name="csrf_token"]`), "forged"),
		chromedp.Click(logout),
	))

	assert.Eventually(t, func() bool { return pageShows(ctx, "The form has expired. Reload the page and try again.") },
		time.Minute, 100*time.Millisecond, "the browser shows the refusal")
	assert.Equal(t, 1, env.countRows(t, "ui_sessions"), "the session is kept")

	require.NoError(t, chromedp.Do(ctx, openServers))

	title, err = chromedp.Run(ctx, chromedp.Title())
	require.NoError(t, err)
	assert.Equal(t, "Servers · toolgate", title, "still signed in")

	// The same form with its token signs out.
	require.NoError(t, chromedp.Do(ctx, chromedp.Click(logout)))

	assert.Eventually(t, func() bool { return pageShows(ctx, "You are signed out of toolgate.") },
		time.Minute, 100*time.Millisecond, "the browser shows the sign-out")
	assert.Equal(t, 0, env.countRows(t, "ui_sessions"))
}
