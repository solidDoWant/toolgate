//go:build integration

package ui_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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

// auditRow is an audit event to insert.
type auditRow struct {
	at        time.Time
	principal string
	workspace string
	session   string
	server    string
	tool      string
}

// insertAudit stores an event and returns its page key.
func (e *uiEnv) insertAudit(t *testing.T, row auditRow) string {
	t.Helper()

	require.NoError(t, store.EnsureAuditPartitions(t.Context(), e.pool, row.at))

	var id int64

	err := e.pool.QueryRow(t.Context(),
		`INSERT INTO audit_events (occurred_at, type, principal, workspace, session, server, tool, details)
		 VALUES ($1, 'tool.call', $2, $3, $4, $5, $6, '{"outcome":"allow"}') RETURNING id`,
		row.at, row.principal, row.workspace, row.session, row.server, row.tool).Scan(&id)
	require.NoError(t, err)

	return fmt.Sprintf("%d-%d", row.at.UnixMicro(), id)
}

var toolCell = regexp.MustCompile(`(?s)<tr>(?:<td>[^<]*</td>){6}<td>([^<]*)</td>`)

// toolsOn lists the tool column of the events a page shows, in order.
func toolsOn(page string) []string {
	var tools []string

	for _, match := range toolCell.FindAllStringSubmatch(strings.ReplaceAll(page, "\n", ""), -1) {
		tools = append(tools, match[1])
	}

	return tools
}

func auditPageOf(t *testing.T, browser *http.Client, base string, query url.Values) (int, string) {
	t.Helper()

	response := get(t, browser, base+"/ui/audit?"+query.Encode())

	return response.StatusCode, readBody(t, response)
}

func TestAuditPageFilters(t *testing.T) {
	env := startUIEnv(t, "admins")
	now := time.Now().UTC().Truncate(time.Microsecond)

	env.insertAudit(t, auditRow{at: now.Add(-1 * time.Hour), principal: "dave", workspace: "w1", session: "s1", server: "github", tool: "dave-1"})
	env.insertAudit(t, auditRow{at: now.Add(-2 * time.Hour), principal: "dave", workspace: "w2", session: "s2", server: "wiki", tool: "dave-2"})
	env.insertAudit(t, auditRow{at: now.Add(-3 * time.Hour), principal: "erin", workspace: "w1", session: "s3", server: "github", tool: "erin-1"})
	env.insertAudit(t, auditRow{at: now.Add(-30 * time.Hour), principal: "dave", workspace: "w1", session: "s1", server: "github", tool: "dave-old"})

	user := env.insertSession(t, "audit-user", nil)
	admin := env.insertSession(t, "audit-admin", []string{"admins"})

	tests := []struct {
		name    string
		browser *http.Client
		query   url.Values
		want    []string
	}{
		{name: "user default range, newest first", browser: user, want: []string{"dave-1", "dave-2"}},
		{name: "user principal parameter is ignored", browser: user, query: url.Values{"principal": {"erin"}}, want: []string{"dave-1", "dave-2"}},
		{name: "admin sees all", browser: admin, want: []string{"dave-1", "dave-2", "erin-1"}},
		{name: "admin by principal", browser: admin, query: url.Values{"principal": {"erin"}}, want: []string{"erin-1"}},
		{name: "workspace", browser: admin, query: url.Values{"workspace": {"w2"}}, want: []string{"dave-2"}},
		{name: "session", browser: admin, query: url.Values{"session": {"s3"}}, want: []string{"erin-1"}},
		{name: "server", browser: admin, query: url.Values{"server": {"wiki"}}, want: []string{"dave-2"}},
		{name: "tool", browser: admin, query: url.Values{"tool": {"dave-1"}}, want: []string{"dave-1"}},
		{name: "tool is exact", browser: admin, query: url.Values{"tool": {"dave"}}, want: nil},
		{name: "two filters", browser: admin, query: url.Values{"server": {"github"}, "workspace": {"w1"}}, want: []string{"dave-1", "erin-1"}},
		{
			name: "wider range", browser: admin,
			query: url.Values{"from": {now.Add(-48 * time.Hour).Format(time.RFC3339)}},
			want:  []string{"dave-1", "dave-2", "erin-1", "dave-old"},
		},
		{
			name: "narrow range", browser: admin,
			query: url.Values{"from": {now.Add(-150 * time.Minute).Format(time.RFC3339)}, "to": {now.Add(-90 * time.Minute).Format(time.RFC3339)}},
			want:  []string{"dave-2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, page := auditPageOf(t, test.browser, env.server.URL, test.query)
			require.Equal(t, http.StatusOK, status)
			assert.Equal(t, test.want, toolsOn(page))
		})
	}
}

func TestAuditPageTimeRangeLimit(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "audit-range", nil)
	to := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name       string
		query      url.Values
		wantStatus int
	}{
		{name: "exactly 31 days", query: url.Values{"to": {to.Format(time.RFC3339)}, "from": {to.Add(-ui.MaxAuditRange).Format(time.RFC3339)}}, wantStatus: http.StatusOK},
		{name: "one second over", query: url.Values{"to": {to.Format(time.RFC3339)}, "from": {to.Add(-ui.MaxAuditRange - time.Second).Format(time.RFC3339)}}, wantStatus: http.StatusBadRequest},
		{name: "empty range", query: url.Values{"to": {to.Format(time.RFC3339)}, "from": {to.Format(time.RFC3339)}}, wantStatus: http.StatusBadRequest},
		{name: "not a time", query: url.Values{"from": {"yesterday"}}, wantStatus: http.StatusBadRequest},
		{name: "bad page marker", query: url.Values{"before": {"x"}}, wantStatus: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, _ := auditPageOf(t, browser, env.server.URL, test.query)
			assert.Equal(t, test.wantStatus, status)
		})
	}
}

func TestAuditPagePages(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "audit-paging", nil)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 101 events, two of them at the same instant, so the id breaks the tie.
	for i := range ui.AuditPageSize + 1 {
		at := now.Add(-time.Duration(i+1) * time.Minute)
		if i == ui.AuditPageSize {
			at = now.Add(-time.Duration(i) * time.Minute)
		}

		env.insertAudit(t, auditRow{at: at, principal: "dave", tool: fmt.Sprintf("t%03d", i)})
	}

	status, first := auditPageOf(t, browser, env.server.URL, nil)
	require.Equal(t, http.StatusOK, status)

	tools := toolsOn(first)
	require.Len(t, tools, ui.AuditPageSize)
	assert.Equal(t, "t000", tools[0])
	assert.Equal(t, "t100", tools[ui.AuditPageSize-1], "of the two events at one instant, the larger id comes first")

	next := regexp.MustCompile(`id="next" href="([^"]*)"`).FindStringSubmatch(first)
	require.NotNil(t, next, "a link to the next page")

	response := get(t, browser, env.server.URL+strings.ReplaceAll(next[1], "&amp;", "&"))
	require.Equal(t, http.StatusOK, response.StatusCode)

	second := readBody(t, response)
	assert.Equal(t, []string{"t099"}, toolsOn(second))
	assert.NotContains(t, second, `id="next"`)
}

func TestAuditEventPage(t *testing.T) {
	env := startUIEnv(t, "admins")
	now := time.Now().UTC().Truncate(time.Microsecond)

	own := env.insertAudit(t, auditRow{at: now.Add(-time.Hour), principal: "dave", tool: "mine", server: "github"})
	other := env.insertAudit(t, auditRow{at: now.Add(-time.Hour), principal: "erin", tool: "hers"})

	user := env.insertSession(t, "event-user", nil)
	admin := env.insertSession(t, "event-admin", []string{"admins"})

	tests := []struct {
		name       string
		browser    *http.Client
		key        string
		wantStatus int
		wantText   string
	}{
		{name: "own event", browser: user, key: own, wantStatus: http.StatusOK, wantText: "mine"},
		{name: "admin sees another's", browser: admin, key: other, wantStatus: http.StatusOK, wantText: "hers"},
		{name: "another's event is 404", browser: user, key: other, wantStatus: http.StatusNotFound},
		{name: "no such event is 404", browser: user, key: "1-1", wantStatus: http.StatusNotFound},
		{name: "bad key is 404", browser: user, key: "abc", wantStatus: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := get(t, test.browser, env.server.URL+"/ui/audit/events/"+test.key)
			require.Equal(t, test.wantStatus, response.StatusCode)

			text := readBody(t, response)
			if test.wantStatus == http.StatusOK {
				assert.Contains(t, text, test.wantText)
				assert.Contains(t, text, "allow", "the details")
			} else {
				assert.NotContains(t, text, "hers")
			}
		})
	}
}

func TestAuditPagesNeedASession(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.browser(t)
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for _, path := range []string{"/ui/audit", "/ui/audit/events/1-1"} {
		response := get(t, browser, env.server.URL+path)
		assert.Equal(t, http.StatusSeeOther, response.StatusCode, path)
		assert.Contains(t, response.Header.Get("Location"), ui.LoginPath)
	}
}

// toolsInBrowser lists the tool column of the events the browser shows.
func toolsInBrowser(ctx context.Context) []string {
	tools, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](
		`Array.from(document.querySelectorAll('#events tbody tr')).map(row => row.cells[6].innerText)`))
	if err != nil {
		return nil
	}

	return tools
}

// [AC-2-05] In a browser, filtering the audit log by principal, workspace,
// session, server, tool and time returns exactly the matching events.
func TestAC2_05AuditFilters(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.insertSession(t, "browser-audit", []string{"admins"})

	now := time.Now().UTC().Truncate(time.Microsecond)

	env.insertAudit(t, auditRow{at: now.Add(-1 * time.Hour), principal: "dave", workspace: "w1", session: "s1", server: "github", tool: "e1"})
	env.insertAudit(t, auditRow{at: now.Add(-2 * time.Hour), principal: "erin", workspace: "w2", session: "s1", server: "github", tool: "e2"})
	env.insertAudit(t, auditRow{at: now.Add(-3 * time.Hour), principal: "erin", workspace: "w1", session: "s2", server: "wiki", tool: "e3"})
	env.insertAudit(t, auditRow{at: now.Add(-4 * time.Hour), principal: "dave", workspace: "w2", session: "s2", server: "wiki", tool: "e4"})
	env.insertAudit(t, auditRow{at: now.Add(-30 * time.Hour), principal: "dave", workspace: "w1", session: "s1", server: "github", tool: "e5"})

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 2*time.Minute)
	defer cancel()

	secure := true
	_, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{
		Name: ui.SessionCookie, Value: "browser-audit", URL: env.server.URL + "/", Path: "/", Secure: &secure, HTTPOnly: &secure,
	})
	require.NoError(t, err)

	submit := chromedp.CSS(`#filter button`)
	// Chromium can report a network change while it starts; try again.
	require.Eventually(t, func() bool {
		return chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+"/ui/audit"), chromedp.WaitVisible(submit)) == nil
	}, time.Minute, 200*time.Millisecond, "the audit page opens")

	fields := []string{"principal", "workspace", "session", "server", "tool", "from", "to"}

	tests := []struct {
		name   string
		fields map[string]string
		want   []string
	}{
		{name: "no filter", want: []string{"e1", "e2", "e3", "e4"}},
		{name: "principal", fields: map[string]string{"principal": "erin"}, want: []string{"e2", "e3"}},
		{name: "workspace", fields: map[string]string{"workspace": "w1"}, want: []string{"e1", "e3"}},
		{name: "session", fields: map[string]string{"session": "s2"}, want: []string{"e3", "e4"}},
		{name: "server", fields: map[string]string{"server": "wiki"}, want: []string{"e3", "e4"}},
		{name: "tool", fields: map[string]string{"tool": "e2"}, want: []string{"e2"}},
		{name: "time", fields: map[string]string{
			"from": now.Add(-210 * time.Minute).Format("2006-01-02T15:04"),
			"to":   now.Add(-90 * time.Minute).Format("2006-01-02T15:04"),
		}, want: []string{"e2", "e3"}},
		{name: "earlier start", fields: map[string]string{"from": now.Add(-48 * time.Hour).Format("2006-01-02T15:04")}, want: []string{"e1", "e2", "e3", "e4", "e5"}},
		{name: "all fields", fields: map[string]string{
			"principal": "dave", "workspace": "w2", "session": "s2", "server": "wiki", "tool": "e4",
		}, want: []string{"e4"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Set every field, so values from an earlier case don't stay.
			for _, field := range fields {
				_, err := chromedp.Run(ctx, chromedp.Evaluate[any](fmt.Sprintf(
					`document.querySelector('#filter input[name=%q]').value = %q`, field, test.fields[field])))
				require.NoError(t, err)
			}

			require.NoError(t, chromedp.Do(ctx, chromedp.Click(submit)))

			// The submitted form is the page the browser is on.
			assert.Eventually(t, func() bool {
				location, err := chromedp.Run(ctx, chromedp.Location())
				if err != nil {
					return false
				}

				parsed, err := url.Parse(location)
				if err != nil {
					return false
				}

				for _, field := range fields {
					if !parsed.Query().Has(field) || parsed.Query().Get(field) != test.fields[field] {
						return false
					}
				}

				return true
			}, time.Minute, 50*time.Millisecond, "the form was submitted")

			assert.Eventually(t, func() bool {
				return fmt.Sprint(toolsInBrowser(ctx)) == fmt.Sprint(test.want)
			}, time.Minute, 100*time.Millisecond, "the browser shows exactly %v", test.want)
		})
	}
}
