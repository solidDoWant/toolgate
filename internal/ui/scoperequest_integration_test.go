//go:build integration

package ui_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// requestScope inserts a pending scope request of dave's, asked by his
// session asker.
func (e *uiEnv) requestScope(t *testing.T, target gateway.ScopeTarget, items ...string) string {
	t.Helper()

	identity := daveIdentity()
	identity.Session = "asker"

	result, err := e.workspaces.RequestScope(t.Context(), identity, target, items, "")
	require.NoError(t, err)
	require.NotEmpty(t, result.Request)

	return result.Request
}

// The approval page shows a scope request with its target, a before-and-
// after list and the asking session, and applies it only after a fresh
// step-up.
func TestScopeRequestApprovalPage(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.addGrantTemplates()

	t.Run("another user, admins too, gets 404", func(t *testing.T) {
		id := env.requestScope(t, gateway.ScopeTarget{Workspace: workspaceName}, "any-server__status")

		for _, groups := range [][]string{{"dev"}, {"dev", "admins"}} {
			cookie := "scope-other-" + groups[len(groups)-1]
			browser := env.insertSessionOf(t, "frank", cookie, groups)
			env.freshSignIn(t, cookie)

			assert.Equal(t, http.StatusNotFound, get(t, browser, env.server.URL+ui.ApprovalPath(id)).StatusCode)
			assert.Equal(t, http.StatusNotFound, post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve",
				url.Values{ui.CSRFField: {csrfTokenOf(cookie)}}).StatusCode)
		}

		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
	})

	t.Run("approving needs a fresh step-up, and nothing changes before it", func(t *testing.T) {
		sessionPK := env.openAttestedSession(t, "dave", "stale-target")
		id := env.requestScope(t, gateway.ScopeTarget{Session: "stale-target"}, "any-server")

		browser := env.insertSession(t, "scope-stale", []string{"dev"})
		env.freshSignIn(t, "scope-stale")
		env.ageSignIn(t, "scope-stale", ui.StepUpMaxAge+30*time.Second)

		page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "Sign in again to continue")

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("scope-stale")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.LoginPath, location(t, response).Path)
		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
		assert.Empty(t, env.additionsOf(t, sessionPK))
	})

	t.Run("a session target shows the target, the asking session and before and after; approving adds the server", func(t *testing.T) {
		sessionPK := env.openAttestedSession(t, "dave", "target-session")
		id := env.requestScope(t, gateway.ScopeTarget{Session: "target-session"}, "any-server")

		browser := env.insertSession(t, "scope-owner", []string{"dev"})
		env.freshSignIn(t, "scope-owner")

		response := get(t, browser, env.server.URL+ui.ApprovalPath(id))
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, "<dt>Target session</dt><dd>target-session</dd>")
		assert.Contains(t, page, "<dt>In workspace</dt><dd>"+additionWorkspace+"</dd>")
		assert.Contains(t, page, "<dt>Asked by session</dt><dd>asker</dd>")
		assert.Contains(t, page, `<td id="change-before"><p>Allowed: nothing by name</p>`)
		assert.Contains(t, page, `<td id="change-after"><p>Allowed: </p><ul class="allowed"><li>any-server</li></ul>`)
		assert.Empty(t, env.additionsOf(t, sessionPK), "nothing changes before approval")

		response = post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("scope-owner")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, "approved", env.accessRequestStatus(t, id))
		assert.Equal(t, []string{"any-server"}, env.additionsOf(t, sessionPK))

		page = readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "This request was approved.")
		assert.NotContains(t, page, "/approve")
	})

	t.Run("row 202: a session target that moved rolls back with the reason on the page", func(t *testing.T) {
		sessionPK := env.openAttestedSession(t, "dave", "moved-session")
		id := env.requestScope(t, gateway.ScopeTarget{Session: "moved-session"}, "any-server")

		_, err := env.pool.Exec(t.Context(), `UPDATE sessions SET workspace = 'coder/dave/elsewhere' WHERE id = $1`, sessionPK)
		require.NoError(t, err)

		browser := env.insertSession(t, "scope-moved", []string{"dev"})
		env.freshSignIn(t, "scope-moved")

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("scope-moved")}})
		require.Equal(t, http.StatusConflict, response.StatusCode)
		assert.Contains(t, readBody(t, response), "This session is now in another workspace.")
		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
		assert.Empty(t, env.additionsOf(t, sessionPK))
	})

	t.Run("a workspace tool target shows the tool after approval as an allow", func(t *testing.T) {
		_, err := env.pool.Exec(t.Context(), `UPDATE user_scopes SET verified_default = 'none' WHERE principal = 'dave'`)
		require.NoError(t, err)

		id := env.requestScope(t, gateway.ScopeTarget{Workspace: "coder/dave/tools"}, "any-server__status")

		browser := env.insertSession(t, "scope-tool", []string{"dev"})
		env.freshSignIn(t, "scope-tool")

		page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "<dt>Target workspace</dt><dd>coder/dave/tools</dd>")
		assert.Contains(t, page, `<li>any-server__status</li>`)

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("scope-tool")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		scopes := env.scopesOf(t, "dave")
		assert.Equal(t, []string{"any-server__status"}, scopes.Entries["coder/dave/tools"].Allowed)
	})
}
