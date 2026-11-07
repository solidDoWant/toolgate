//go:build integration

package ui_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// sidebarLinks returns the sidebar's group headings and link targets in
// page order.
func sidebarLinks(page string) []string {
	start := strings.Index(page, `<nav aria-label="Main"`)
	end := strings.Index(page, "</nav>")

	if start < 0 || end < start {
		return nil
	}

	pattern := regexp.MustCompile(`uppercase">(\w+)</p>|href="(/ui/[a-z]+)"`)

	var items []string

	for _, match := range pattern.FindAllStringSubmatch(page[start:end], -1) {
		items = append(items, match[1]+match[2])
	}

	return items
}

func TestShell(t *testing.T) {
	t.Run("the sidebar groups the pages, with Admin only for admins", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, _ := env.adminBrowser(t)
		user := env.insertSessionOf(t, "erin", "user-session", []string{"dev"})

		use := []string{"Use", "/ui/servers", "/ui/sessions", "/ui/workspaces", "/ui/secrets", "/ui/setup"}
		review := []string{"Review", "/ui/approvals", "/ui/grants", "/ui/audit"}

		page := readBody(t, get(t, admin, env.server.URL+"/ui/audit"))
		assert.Equal(t, append(append(append([]string{}, use...), review...), "Admin", "/ui/blocks"), sidebarLinks(page))
		assert.Contains(t, page, `href="/ui/audit" class=`)
		assert.Regexp(t, `href="/ui/audit"[^>]* aria-current="page"`, page)

		page = readBody(t, get(t, user, env.server.URL+"/ui/audit"))
		assert.Equal(t, append(append([]string{}, use...), review...), sidebarLinks(page))
	})

	t.Run("the approvals link counts the user's pending approvals", func(t *testing.T) {
		env := startUIEnv(t, "")
		browser := env.insertSession(t, "dave-session", nil)

		badge := regexp.MustCompile(`href="/ui/approvals"[^>]*>Approvals <span[^>]*>(\d+)</span>`)

		assert.NotRegexp(t, badge, readBody(t, get(t, browser, env.server.URL+"/ui/servers")), "no badge without approvals")

		env.insertApproval(t, approvalID(1), "dave", time.Hour)
		env.insertApproval(t, approvalID(2), "dave", time.Hour)
		env.insertApproval(t, approvalID(3), "dave", -time.Second)
		env.insertApproval(t, approvalID(4), "erin", time.Hour)

		match := badge.FindStringSubmatch(readBody(t, get(t, browser, env.server.URL+"/ui/servers")))
		require.NotNil(t, match, "a badge with pending approvals")
		assert.Equal(t, "2", match[1], "only dave's live approvals count")
	})

	t.Run("every page renders inside the shell", func(t *testing.T) {
		env := startUIEnv(t, "toolgate-admins")
		admin, _ := env.adminBrowser(t)

		for _, path := range []string{
			"/ui/servers", "/ui/sessions", "/ui/workspaces", "/ui/secrets", "/ui/setup",
			"/ui/approvals", "/ui/grants", "/ui/audit", "/ui/blocks", "/ui/blocks/new",
		} {
			response := get(t, admin, env.server.URL+path)
			require.Equal(t, http.StatusOK, response.StatusCode, path)

			page := readBody(t, response)
			assert.Contains(t, page, `<aside id="sidebar"`, path)
			assert.Contains(t, page, `<form method="post" action="/ui/logout"`, path)
			assert.Equal(t, 1, strings.Count(page, "<h1"), "%s has one heading: %s", path, regexp.MustCompile(`<h1[^<]*`).FindAllString(page, -1))
		}
	})
}

// openBrowserAsAdmin starts a browser signed in with a stored admin session.
func openBrowserAsAdmin(t *testing.T, env *uiEnv) context.Context {
	t.Helper()

	env.adminBrowser(t)

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 3*time.Minute)
	t.Cleanup(cancel)

	secure := true
	_, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{
		Name: ui.SessionCookie, Value: "admin-session", URL: env.server.URL + "/", Path: "/", Secure: &secure, HTTPOnly: &secure,
	})
	require.NoError(t, err)

	return ctx
}

// activeElement returns the id, or else the text, of the focused element.
func activeElement(ctx context.Context) (string, error) {
	return chromedp.Run(ctx, chromedp.Evaluate[string](`document.activeElement.id || document.activeElement.textContent.trim()`))
}

func TestNewBlockDialogWorksFromTheKeyboard(t *testing.T) {
	env := startUIEnv(t, "toolgate-admins")
	ctx := openBrowserAsAdmin(t, env)

	newBlock := `main a[data-dialog="new-block"]`
	dialogOpen := func() bool {
		open, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`document.getElementById("new-block").open`))
		require.NoError(t, err)

		return open
	}

	require.NoError(t, chromedp.Do(ctx,
		toolgatetest.Navigate(env.server.URL+"/ui/blocks"),
		chromedp.WaitVisible(newBlock),
		chromedp.Focus(newBlock),
		chromedp.KeyEvent(kb.Enter),
	))

	require.True(t, dialogOpen(), "Enter on New block opens the dialog")

	textareaShown, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`document.getElementById("field-servers-items").offsetParent !== null`))
	require.NoError(t, err)
	assert.False(t, textareaShown, "a token input shows chips and a text box, not its textarea")

	focused, err := activeElement(ctx)
	require.NoError(t, err)
	assert.Equal(t, "field-principals", focused, "the dialog's first field has the focus")

	// Tab from the last control wraps to the first; Shift+Tab goes back.
	require.NoError(t, chromedp.Do(ctx, chromedp.Focus(`#new-block button[type="submit"]`), chromedp.KeyEvent(kb.Tab)))

	focused, err = activeElement(ctx)
	require.NoError(t, err)
	assert.Equal(t, "×", focused, "Tab stays inside the dialog")

	require.NoError(t, chromedp.Do(ctx, chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(chromedp.ModifierShift))))

	focused, err = activeElement(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Add block", focused, "Shift+Tab stays inside the dialog")

	// Escape closes the dialog and gives the focus back to its link.
	require.NoError(t, chromedp.Do(ctx, chromedp.KeyEvent(kb.Escape)))
	require.Eventually(t, func() bool { return !dialogOpen() }, 10*time.Second, 50*time.Millisecond)

	linkFocused, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`document.activeElement === document.querySelector('`+newBlock+`')`))
	require.NoError(t, err)
	assert.True(t, linkFocused, "the focus returns to New block")

	// Open it again and fill it in from the keyboard: two servers as
	// tokens, the reason, and Enter on the submit button.
	require.NoError(t, chromedp.Do(ctx,
		chromedp.KeyEvent(kb.Enter),
		chromedp.Focus(`#field-servers`),
		chromedp.KeyEvent("github"+kb.Enter+"wiki"+kb.Enter),
		chromedp.Focus(`#field-reason`),
		chromedp.KeyEvent("Incident 9."),
		chromedp.Focus(`#new-block button[type="submit"]`),
		chromedp.KeyEvent(kb.Enter),
	))

	require.Eventually(t, func() bool { return env.countRows(t, "temporary_blocks") == 1 }, time.Minute, 100*time.Millisecond)

	var match, reason string
	require.NoError(t, env.pool.QueryRow(t.Context(), `SELECT match::text, reason FROM temporary_blocks`).Scan(&match, &reason))
	assert.JSONEq(t, `{"servers":["github","wiki"]}`, match)
	assert.Equal(t, "Incident 9.", reason)
	assert.Eventually(t, func() bool { return pageShows(ctx, "servers: github, wiki") }, time.Minute, 100*time.Millisecond)
}
