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

	require.NoError(t, store.CreateAuditPartitions(t.Context(), e.pool, at))

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
		// The time and outcome cells hold tags, so only plain cells match.
		if len(cells) >= 2 {
			tools = append(tools, cells[1][1])
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

	// 51 decisions: the page shows the newest 50.
	for index := range 51 {
		env.insertDecision(t, "tool.decision", "dave", "mine", fmt.Sprintf("tool-%02d", index), now.Add(-time.Duration(60-index)*time.Minute))
	}

	env.insertDecision(t, "tool.call", "dave", "mine", "not-a-decision", now.Add(-time.Minute))
	env.insertDecision(t, "tool.decision", "erin", "mine", "other-principal", now.Add(-time.Minute))
	env.insertDecision(t, "tool.decision", "dave", "other-session", "other-session", now.Add(-time.Minute))

	list := func(browser *http.Client) string {
		response := get(t, browser, env.server.URL+"/ui/sessions?user=all")
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

	t.Run("a session shows its servers and last 50 calls", func(t *testing.T) {
		response := get(t, user, fmt.Sprintf("%s/ui/sessions/%d", env.server.URL, mine))
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, ">default/github</a>")
		assert.Contains(t, page, ">default/wiki</a>")

		var want []string
		for index := 50; index >= 1; index-- {
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

// In a browser, a user sees their open sessions with the enabled
// servers and last decisions, gets 404 for another user's session, and an
// admin sees all.
func TestSessionsPage(t *testing.T) {
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

			response, err := chromedp.RunResponse(ctx, toolgatetest.Navigate(env.server.URL+path))
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
	require.Equal(t, int64(200), open(t, "/ui/sessions?user=all"))
	assert.ElementsMatch(t, []string{mineURL, otherURL}, links())
	assert.Equal(t, int64(200), open(t, otherURL))
}

// insertCallEvent stores an audit event of a session with details.
func (e *uiEnv) insertCallEvent(t *testing.T, eventType, principal, sessionID, server, tool, details string, at time.Time) {
	t.Helper()

	require.NoError(t, store.CreateAuditPartitions(t.Context(), e.pool, at))

	_, err := e.pool.Exec(t.Context(),
		`INSERT INTO audit_events (occurred_at, type, principal, session, server, tool, details)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		at, eventType, principal, sessionID, server, tool, details)
	require.NoError(t, err)
}

func TestSessionPageSections(t *testing.T) {
	env := startUIEnv(t, "admins")
	user := env.insertSession(t, "sections-user", nil)

	id := env.insertGatewaySession(t, "dave", "sections", false, "github", "prod-db")
	now := time.Now().UTC()

	_, err := env.pool.Exec(t.Context(),
		`UPDATE sessions SET workspace = 'coder/dave/ws', attestation = 'attested', tool_search = true WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = env.pool.Exec(t.Context(),
		`INSERT INTO mcp_sessions (id, session_pk, principal, session_id, workspace, attestation, protocol_version, initialize_params)
		 VALUES ('m1', $1, 'dave', 'sections', 'coder/dave/ws', 'attested', '2025-06-18', '{"clientInfo":{"name":"Claude Code","version":"2.1.319"}}'::jsonb)`, id)
	require.NoError(t, err)
	_, err = env.pool.Exec(t.Context(), `INSERT INTO session_additions (session_pk, server, request_id) VALUES ($1, 'extra-server', 'r1')`, id)
	require.NoError(t, err)

	env.insertCallEvent(t, "tool.decision", "dave", "sections", "github", "refused-tool", `{"decision":"refuse","reason":"no grant for this tool"}`, now.Add(-5*time.Minute))
	env.insertCallEvent(t, "tool.result", "dave", "sections", "github", "slow-tool", `{"outcome":"ok","duration_ms":1234}`, now.Add(-4*time.Minute))
	env.insertCallEvent(t, "approval.request", "dave", "sections", "github", "needs-approval", `{"approval":"a1"}`, now.Add(-3*time.Minute))
	env.insertCallEvent(t, "tool.decision", "dave", "sections", "github", "approved-tool", `{"decision":"allow","approval":"a1"}`, now.Add(-2*time.Minute))
	env.insertCallEvent(t, "tool.search", "dave", "sections", "", "", `{}`, now.Add(-time.Minute))

	response := get(t, user, fmt.Sprintf("%s/ui/sessions/%d", env.server.URL, id))
	require.Equal(t, http.StatusOK, response.StatusCode)

	page := readBody(t, response)

	t.Run("the header shows where, client and tool search", func(t *testing.T) {
		assert.Contains(t, page, "Coder workspace ws")
		assert.Contains(t, page, "verified")
		assert.Contains(t, page, "Claude Code 2.1.319")
		assert.Contains(t, page, "helper")
		assert.Contains(t, page, "<dt>Tool search</dt><dd>on</dd>")
	})

	t.Run("servers show their status and instance", func(t *testing.T) {
		assert.Regexp(t, `default/github</a></td>\s*<td><span class="badge badge-ok">ready</span>`, page)
		assert.Regexp(t, `<span class="badge badge-bad">blocked</span> Only from an attested environment`, page)
		assert.Contains(t, page, "namespace default")
	})

	t.Run("the effective scope shows additions and links to the changing pages", func(t *testing.T) {
		assert.Contains(t, page, `id="scope"`)
		assert.Contains(t, page, "<code>extra-server</code>")
		assert.Contains(t, page, `href="/ui/workspaces"`)
	})

	t.Run("recent calls show outcome, latency, reason and approvals", func(t *testing.T) {
		assert.Contains(t, page, "no grant for this tool")
		assert.Contains(t, page, "1234 ms")
		assert.Contains(t, page, `badge badge-bad">refused`)
		assert.Contains(t, page, `badge badge-warn">pending`)
		assert.Contains(t, page, `badge badge-ok">approved`)
		assert.NotContains(t, page, "tool.search")
		assert.Contains(t, page, `href="/ui/audit?session=sections"`)
	})
}
