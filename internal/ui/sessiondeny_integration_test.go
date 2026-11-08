//go:build integration

package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/ui"
)

func TestSessionDenyFromSessionPage(t *testing.T) {
	env := startUIEnv(t, "admins")
	owner := env.insertSession(t, "deny-owner", nil)
	other := env.insertSessionOf(t, "erin", "deny-other", nil)
	admin := env.insertSessionOf(t, "root", "deny-admin", []string{"admins"})
	token := csrfTokenOf("deny-owner")

	id := env.insertGatewaySession(t, "dave", "deny-session", false, "github", "wiki")
	sibling := env.insertGatewaySession(t, "dave", "deny-sibling", false, "github")
	pageURL := fmt.Sprintf("%s/ui/sessions/%d", env.server.URL, id)
	denyURL := pageURL + "/deny"

	deniedEvents := func() int {
		var count int
		require.NoError(t, env.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_events WHERE type = 'session.addition' AND session = 'deny-session'
			 AND server = 'github' AND details->>'denied' = 'true'`).Scan(&count))

		return count
	}

	t.Run("the page has a plain form for each server and no end action", func(t *testing.T) {
		page := readBody(t, get(t, owner, pageURL))
		assert.Contains(t, page, `<form method="get" action="/ui/sessions/`+fmt.Sprint(id)+`/deny" class="m-0"><input type="hidden" name="server" value="github">`)
		assert.Contains(t, page, `name="server" value="wiki"`)
		assert.NotContains(t, page, "End session")
	})

	t.Run("an admin who is not the owner sees no form and gets 404 from both routes", func(t *testing.T) {
		response := get(t, admin, pageURL)
		require.Equal(t, http.StatusOK, response.StatusCode)
		assert.NotContains(t, readBody(t, response), "Deny for this session")

		assert.Equal(t, http.StatusNotFound, get(t, admin, denyURL+"?server=github").StatusCode)
		assert.Equal(t, http.StatusNotFound, post(t, admin, denyURL, url.Values{ui.CSRFField: {csrfTokenOf("deny-admin")}, "server": {"github"}}).StatusCode)
		assert.Equal(t, http.StatusNotFound, get(t, other, denyURL+"?server=github").StatusCode)
		assert.Equal(t, http.StatusNotFound, post(t, other, denyURL, url.Values{ui.CSRFField: {csrfTokenOf("deny-other")}, "server": {"github"}}).StatusCode)
		assert.Zero(t, deniedEvents())
	})

	t.Run("a post without the CSRF token is refused", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, post(t, owner, denyURL, url.Values{"server": {"github"}}).StatusCode)
		assert.Zero(t, deniedEvents())
	})

	t.Run("a server the session has not enabled is 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, owner, denyURL+"?server=jira").StatusCode)
		assert.Equal(t, http.StatusNotFound, post(t, owner, denyURL, url.Values{ui.CSRFField: {token}, "server": {"jira"}}).StatusCode)
	})

	t.Run("the confirmation page changes nothing", func(t *testing.T) {
		response := get(t, owner, denyURL+"?server=github")
		require.Equal(t, http.StatusOK, response.StatusCode)
		assert.Contains(t, readBody(t, response), `name="csrf_token" value="`+token+`"`)
		assert.Zero(t, deniedEvents())
	})

	t.Run("a confirmed deny blocks the server for that session only and writes the audit event", func(t *testing.T) {
		response := post(t, owner, denyURL, url.Values{ui.CSRFField: {token}, "server": {"github"}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, fmt.Sprintf("/ui/sessions/%d", id), location(t, response).Path)

		assert.Equal(t, 1, deniedEvents())

		page := readBody(t, get(t, owner, pageURL))
		assert.Contains(t, page, "blocked for this session")
		assert.Equal(t, []string{"github"}, env.deniedEntries(t, id))
		assert.Empty(t, env.deniedEntries(t, sibling))
	})
}

// deniedEntries returns the server names that the session's deny entries
// name.
func (e *uiEnv) deniedEntries(t *testing.T, sessionPK int64) []string {
	t.Helper()

	rows, err := e.pool.Query(t.Context(), `SELECT server FROM scope_entries WHERE session_pk = $1 AND effect = 'deny' ORDER BY server`, sessionPK)
	require.NoError(t, err)

	defer rows.Close()

	var servers []string

	for rows.Next() {
		var server string
		require.NoError(t, rows.Scan(&server))

		servers = append(servers, server)
	}

	return servers
}
