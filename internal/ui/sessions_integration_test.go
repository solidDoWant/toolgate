//go:build integration

package ui_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// insertGatewaySession stores an open (or ended) gateway session with its
// enabled servers and returns its row ID.
func (e *uiEnv) insertGatewaySession(t *testing.T, principal, sessionID string, ended bool, servers ...string) int64 {
	t.Helper()

	var id int64

	err := e.pool.QueryRow(t.Context(),
		`INSERT INTO sessions (principal, session_id, workspace, attestation, ended_at, created_at)
		 VALUES ($1, $2, 'ws', 'user', CASE WHEN $3 THEN now() END, now() - interval '2 hours') RETURNING id`,
		principal, sessionID, ended).Scan(&id)
	require.NoError(t, err)

	for _, server := range servers {
		_, err := e.pool.Exec(t.Context(),
			`INSERT INTO session_servers (session_pk, template_ns, template_name) VALUES ($1, 'default', $2)`, id, server)
		require.NoError(t, err)
	}

	return id
}

// insertDecision stores a tool.decision (or another type) event.
func (e *uiEnv) insertDecision(t *testing.T, eventType, principal, sessionID, tool string, at time.Time) {
	t.Helper()

	require.NoError(t, store.EnsureAuditPartitions(t.Context(), e.pool, at))

	_, err := e.pool.Exec(t.Context(),
		`INSERT INTO audit_events (occurred_at, type, principal, session, server, tool, details)
		 VALUES ($1, $2, $3, $4, 'github', $5, '{"decision":"allow"}')`,
		at, eventType, principal, sessionID, tool)
	require.NoError(t, err)
}

var cellOfRow = regexp.MustCompile(`<td>([^<]*)</td>`)

func decisionTools(page string) []string {
	start := strings.Index(page, `id="decisions"`)
	if start < 0 {
		return nil
	}

	var tools []string

	for _, row := range strings.Split(page[start:], "<tr>")[1:] {
		cells := cellOfRow.FindAllStringSubmatch(row, -1)
		if len(cells) >= 3 {
			tools = append(tools, cells[2][1])
		}
	}

	return tools
}

func TestSessionPages(t *testing.T) {
	env := startUIEnv(t, "admins")
	user := env.insertSession(t, "sessions-user", nil)
	admin := env.insertSession(t, "sessions-admin", []string{"admins"})

	now := time.Now().UTC().Truncate(time.Microsecond)
	mine := env.insertGatewaySession(t, "dave", "mine", false, "github", "wiki")
	other := env.insertGatewaySession(t, "erin", "theirs", false, "jira")
	ended := env.insertGatewaySession(t, "dave", "gone", true)

	// 21 decisions: the page shows the newest 20.
	for index := range 21 {
		env.insertDecision(t, "tool.decision", "dave", "mine", fmt.Sprintf("tool-%02d", index), now.Add(-time.Duration(30-index)*time.Minute))
	}

	env.insertDecision(t, "tool.call", "dave", "mine", "not-a-decision", now.Add(-time.Minute))
	env.insertDecision(t, "tool.decision", "erin", "mine", "other-principal", now.Add(-time.Minute))
	env.insertDecision(t, "tool.decision", "dave", "other-session", "other-session", now.Add(-time.Minute))

	list := func(browser *http.Client) string {
		response := get(t, browser, env.server.URL+"/ui/sessions")
		require.Equal(t, http.StatusOK, response.StatusCode)

		return readBody(t, response)
	}

	t.Run("a user sees only their open sessions", func(t *testing.T) {
		page := list(user)
		assert.Contains(t, page, fmt.Sprintf("/ui/sessions/%d\"", mine))
		assert.NotContains(t, page, fmt.Sprintf("/ui/sessions/%d\"", other))
		assert.NotContains(t, page, fmt.Sprintf("/ui/sessions/%d\"", ended))
	})

	t.Run("an admin sees all open sessions", func(t *testing.T) {
		page := list(admin)
		assert.Contains(t, page, fmt.Sprintf("/ui/sessions/%d\"", mine))
		assert.Contains(t, page, fmt.Sprintf("/ui/sessions/%d\"", other))
		assert.NotContains(t, page, fmt.Sprintf("/ui/sessions/%d\"", ended))
	})

	t.Run("a session shows its servers and last 20 decisions", func(t *testing.T) {
		response := get(t, user, fmt.Sprintf("%s/ui/sessions/%d", env.server.URL, mine))
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, "<li>default/github</li>")
		assert.Contains(t, page, "<li>default/wiki</li>")

		var want []string
		for index := 20; index >= 1; index-- {
			want = append(want, fmt.Sprintf("tool-%02d", index))
		}

		assert.Equal(t, want, decisionTools(page))
	})

	t.Run("another user's session is 404 and an admin's view is 200", func(t *testing.T) {
		url := fmt.Sprintf("%s/ui/sessions/%d", env.server.URL, other)
		assert.Equal(t, http.StatusNotFound, get(t, user, url).StatusCode)
		assert.Equal(t, http.StatusOK, get(t, admin, url).StatusCode)
	})

	t.Run("ended and unknown sessions are 404", func(t *testing.T) {
		for _, id := range []string{fmt.Sprint(ended), "999999", "x", "-1"} {
			assert.Equal(t, http.StatusNotFound, get(t, admin, env.server.URL+"/ui/sessions/"+id).StatusCode, id)
		}
	})
}

// [AC-2-04] In a browser, a user sees their open sessions with the enabled
// servers and last decisions, gets 404 for another user's session, and an
// admin sees all.
func TestAC2_04Sessions(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.insertSession(t, "ac-user", nil)
	env.insertSession(t, "ac-admin", []string{"admins"})

	mine := env.insertGatewaySession(t, "dave", "mine", false, "github")
	other := env.insertGatewaySession(t, "erin", "theirs", false, "jira")
	env.insertDecision(t, "tool.decision", "dave", "mine", "my-tool", time.Now().UTC().Add(-time.Minute))

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 2*time.Minute)
	defer cancel()

	signInAs := func(t *testing.T, cookie string) {
		t.Helper()

		secure := true
		_, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{
			Name: ui.SessionCookie, Value: cookie, URL: env.server.URL + "/", Path: "/", Secure: &secure, HTTPOnly: &secure,
		})
		require.NoError(t, err)
	}

	open := func(t *testing.T, path string) (status int64) {
		t.Helper()

		require.Eventually(t, func() bool {
			status = 0

			response, err := chromedp.RunResponse(ctx, chromedp.Navigate(env.server.URL+path))
			if err != nil || response == nil {
				return false
			}

			status = response.Status

			return true
		}, time.Minute, 200*time.Millisecond)

		return status
	}

	links := func() []string {
		hrefs, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](
			`Array.from(document.querySelectorAll('#sessions tbody a')).map(a => a.getAttribute('href'))`))
		require.NoError(t, err)

		return hrefs
	}

	mineURL := fmt.Sprintf("/ui/sessions/%d", mine)
	otherURL := fmt.Sprintf("/ui/sessions/%d", other)

	signInAs(t, "ac-user")
	require.Equal(t, int64(200), open(t, "/ui/sessions"))
	assert.Equal(t, []string{mineURL}, links())
	require.Equal(t, int64(200), open(t, mineURL))

	text, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.body.innerText`))
	require.NoError(t, err)
	assert.Contains(t, text, "default/github")
	assert.Contains(t, text, "my-tool")
	assert.Equal(t, int64(404), open(t, otherURL))

	signInAs(t, "ac-admin")
	require.Equal(t, int64(200), open(t, "/ui/sessions"))
	assert.ElementsMatch(t, []string{mineURL, otherURL}, links())
	assert.Equal(t, int64(200), open(t, otherURL))
}
