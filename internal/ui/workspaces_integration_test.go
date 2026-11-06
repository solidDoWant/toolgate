//go:build integration

package ui_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

const workspaceName = "coder/dave/mail"

// scopesOf returns principal's defaults and lists.
func (e *uiEnv) scopesOf(t *testing.T, principal string) gateway.WorkspaceScopes {
	t.Helper()

	scopes, err := e.workspaces.Scopes(t.Context(), principal)
	require.NoError(t, err)

	return scopes
}

// listOf returns the servers of principal's list for workspace, and whether
// the list exists.
func (e *uiEnv) listOf(t *testing.T, principal, workspace string) ([]string, bool) {
	t.Helper()

	for _, list := range e.scopesOf(t, principal).Lists {
		if list.Workspace == workspace {
			return list.Servers, true
		}
	}

	return nil, false
}

// daveIdentity is dave as a signed-in user in the group dev.
func daveIdentity() auth.Identity {
	return auth.Identity{Principal: "dave", Groups: []string{"dev"}, Kind: auth.KindUser, Attestation: auth.AttestationUser}
}

// requestChange inserts a pending change request of dave's, as the CLI does.
func (e *uiEnv) requestChange(t *testing.T, change gateway.WorkspaceChange) string {
	t.Helper()

	result, err := e.workspaces.Request(t.Context(), daveIdentity(), change)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.NotEmpty(t, result.Request)

	return result.Request
}

func (e *uiEnv) accessRequestStatus(t *testing.T, id string) string {
	t.Helper()

	request, err := db.New(e.pool).GetAccessRequestOfPrincipal(t.Context(), db.GetAccessRequestOfPrincipalParams{ID: id, Principal: "dave"})
	require.NoError(t, err)

	return request.Status
}

// [AC-3-17] The Workspaces page applies a widening change only with a fresh
// step-up, a narrowing one without, and refuses a POST without the CSRF token
// or a UI session.
func TestAC3_17WorkspacesPage(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()

	t.Run("[AC-3-17] the page shows the defaults, lists and open verified workspaces", func(t *testing.T) {
		_, err := db.New(env.pool).CreateSession(t.Context(), db.CreateSessionParams{
			Principal: "dave", SessionID: "attested-1", Workspace: "coder/dave/blog",
			Attestation: auth.AttestationAttested, Kind: auth.KindUser, Groups: []string{},
		})
		require.NoError(t, err)
		_, err = db.New(env.pool).CreateSession(t.Context(), db.CreateSessionParams{
			Principal: "dave", SessionID: "claimed-1", Workspace: "claimed/elsewhere",
			Attestation: auth.AttestationUser, Kind: auth.KindUser, Groups: []string{},
		})
		require.NoError(t, err)

		browser := env.insertSession(t, "ws-page", []string{"dev"})

		response := get(t, browser, env.server.URL+ui.WorkspacesPath)
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, "A workspace&#39;s list limits a process holding your token only while the default for unverified sessions is nothing.")
		assert.Contains(t, page, "coder/dave/blog")
		assert.Contains(t, page, "Add a list")
		assert.NotContains(t, page, "claimed/elsewhere", "only verified workspaces are offered")
		assert.Contains(t, page, "Sign in again to continue", "a sign-in of unknown age asks for a step-up")
	})

	t.Run("[AC-3-17] a widening POST without a fresh step-up is refused", func(t *testing.T) {
		browser := env.insertSession(t, "ws-stale", []string{"dev"})
		env.freshSignIn(t, "ws-stale")
		env.ageSignIn(t, "ws-stale", ui.StepUpMaxAge+30*time.Second)

		response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/allow", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-stale")}, "workspace": {workspaceName}, "server": {"optin-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		stepUp := location(t, response)
		assert.Equal(t, ui.LoginPath, stepUp.Path)
		assert.Equal(t, "1", stepUp.Query().Get("step_up"))
		assert.Equal(t, ui.WorkspacesPath, stepUp.Query().Get("return_to"))

		_, exists := env.listOf(t, "dave", workspaceName)
		assert.False(t, exists, "the stale widening POST changes nothing")

		response = post(t, browser, env.server.URL+ui.WorkspacesPath+"/defaults", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-stale")}, "verified": {"all"}, "unverified": {"none"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.LoginPath, location(t, response).Path)
		assert.Equal(t, gateway.ScopeNone, env.scopesOf(t, "dave").VerifiedDefault)
	})

	t.Run("[AC-3-17] a widening POST with a fresh step-up applies", func(t *testing.T) {
		browser := env.insertSession(t, "ws-fresh", []string{"dev"})
		env.freshSignIn(t, "ws-fresh")

		response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/allow", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-fresh")}, "workspace": {workspaceName}, "server": {"optin-server", "any-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.WorkspacesPath, location(t, response).Path)

		servers, exists := env.listOf(t, "dave", workspaceName)
		require.True(t, exists)
		assert.Equal(t, []string{"any-server", "optin-server"}, servers)

		page := readBody(t, get(t, browser, env.server.URL+ui.WorkspacesPath))
		assert.Contains(t, page, workspaceName)
		assert.Contains(t, page, "optin-server")
	})

	t.Run("[AC-3-17] a narrowing POST applies without a step-up", func(t *testing.T) {
		browser := env.insertSession(t, "ws-narrow", []string{"dev"})
		env.freshSignIn(t, "ws-narrow")
		env.ageSignIn(t, "ws-narrow", time.Hour)

		response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/deny", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-narrow")}, "workspace": {workspaceName}, "server": {"any-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.WorkspacesPath, location(t, response).Path)

		servers, _ := env.listOf(t, "dave", workspaceName)
		assert.Equal(t, []string{"optin-server"}, servers)

		// With the verified default none, clearing a list narrows.
		response = post(t, browser, env.server.URL+ui.WorkspacesPath+"/clear", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-narrow")}, "workspace": {workspaceName},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.WorkspacesPath, location(t, response).Path)

		_, exists := env.listOf(t, "dave", workspaceName)
		assert.False(t, exists)
	})

	t.Run("[AC-3-17] a POST without the CSRF token is refused", func(t *testing.T) {
		browser := env.insertSession(t, "ws-csrf", []string{"dev"})
		env.freshSignIn(t, "ws-csrf")

		for _, action := range []string{"defaults", "allow", "deny", "clear"} {
			response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/"+action, url.Values{
				"workspace": {workspaceName}, "server": {"optin-server"}, "verified": {"all"},
			})
			assert.Equal(t, http.StatusForbidden, response.StatusCode, action)
		}

		_, exists := env.listOf(t, "dave", workspaceName)
		assert.False(t, exists)
		assert.Equal(t, gateway.ScopeNone, env.scopesOf(t, "dave").VerifiedDefault)
	})

	t.Run("[AC-3-17] a request without a UI session is refused", func(t *testing.T) {
		browser := env.browser(t)

		response := get(t, browser, env.server.URL+ui.WorkspacesPath)
		assert.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.LoginPath, location(t, response).Path)

		for _, action := range []string{"defaults", "allow", "deny", "clear"} {
			response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/"+action, url.Values{
				"workspace": {workspaceName}, "server": {"optin-server"}, "verified": {"all"},
			})
			assert.Equal(t, http.StatusUnauthorized, response.StatusCode, action)
		}
	})
}

// [AC-3-17] A change request's page shows the change before and after, only
// to its owner, and approving it with a fresh step-up applies the change.
func TestAC3_17ChangeApprovalPage(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.addGrantTemplates()

	t.Run("[AC-3-17] another user, admins too, gets 404", func(t *testing.T) {
		id := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: workspaceName, Servers: []string{"optin-server"}})

		for _, groups := range [][]string{{"dev"}, {"dev", "admins"}} {
			cookie := "change-other-" + groups[len(groups)-1]
			browser := env.insertSessionOf(t, "frank", cookie, groups)
			env.freshSignIn(t, cookie)

			assert.Equal(t, http.StatusNotFound, get(t, browser, env.server.URL+ui.ApprovalPath(id)).StatusCode)
			assert.Equal(t, http.StatusNotFound, post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve",
				url.Values{ui.CSRFField: {csrfTokenOf(cookie)}}).StatusCode)
			assert.Equal(t, http.StatusNotFound, post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/deny",
				url.Values{ui.CSRFField: {csrfTokenOf(cookie)}}).StatusCode)
		}

		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
	})

	t.Run("[AC-3-17] approving applies the change; then the page offers nothing", func(t *testing.T) {
		browser := env.insertSession(t, "change-owner", []string{"dev"})
		env.freshSignIn(t, "change-owner")

		before := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/blog", Servers: []string{"any-server"}})
		require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/blog", Servers: []string{"optin-server"}}, gateway.ViaUI))

		response := get(t, browser, env.server.URL+ui.ApprovalPath(before))
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, `<td id="change-before"><ul><li>optin-server</li></ul></td>`)
		assert.Contains(t, page, `<td id="change-after"><ul><li>any-server</li><li>optin-server</li></ul></td>`)
		assert.Contains(t, page, "/approve")
		assert.Contains(t, page, "/deny")

		response = post(t, browser, env.server.URL+ui.ApprovalPath(before)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("change-owner")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.ApprovalPath(before), location(t, response).Path)

		servers, _ := env.listOf(t, "dave", "coder/dave/blog")
		assert.Equal(t, []string{"any-server", "optin-server"}, servers)
		assert.Equal(t, "approved", env.accessRequestStatus(t, before))

		page = readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(before)))
		assert.Contains(t, page, "This request was approved.")
		assert.NotContains(t, page, "/approve")
		assert.NotContains(t, page, "/deny")
	})

	t.Run("[AC-3-17] approving needs a fresh step-up", func(t *testing.T) {
		id := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceDefaults, Verified: gateway.ScopeAll})

		browser := env.insertSession(t, "change-stale", []string{"dev"})
		env.freshSignIn(t, "change-stale")
		env.ageSignIn(t, "change-stale", ui.StepUpMaxAge+30*time.Second)

		page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "Sign in again to continue")
		assert.Contains(t, page, "<td>none</td><td>all</td>", "the defaults change shows before and after")

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{ui.CSRFField: {csrfTokenOf("change-stale")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.LoginPath, location(t, response).Path)
		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
		assert.Equal(t, gateway.ScopeNone, env.scopesOf(t, "dave").VerifiedDefault)

		// Deny needs no step-up.
		response = post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/deny", url.Values{ui.CSRFField: {csrfTokenOf("change-stale")}})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, "denied", env.accessRequestStatus(t, id))
		assert.Equal(t, gateway.ScopeNone, env.scopesOf(t, "dave").VerifiedDefault)

		page = readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "This request was denied.")
		assert.NotContains(t, page, "/approve")
	})

	t.Run("[AC-3-17] the approval page needs a UI session", func(t *testing.T) {
		id := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/other", Servers: []string{"optin-server"}})
		browser := env.browser(t)

		response := post(t, browser, env.server.URL+ui.ApprovalPath(id)+"/approve", url.Values{})
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		assert.Equal(t, "pending", env.accessRequestStatus(t, id))
	})
}

func TestWorkspacesPageIsOffWithoutTheWorkspaces(t *testing.T) {
	env := startUIEnvConfigured(t, "", toolgatetest.OIDCOptions{}, func(options *ui.Options) { options.Workspaces = nil })
	browser := env.insertSession(t, "ws-off", []string{"dev"})

	assert.Equal(t, http.StatusNotFound, get(t, browser, env.server.URL+ui.WorkspacesPath).StatusCode)
}
