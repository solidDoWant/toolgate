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

// The Workspaces page applies a widening change only with a fresh
// step-up, a narrowing one without, and refuses a POST without the CSRF token
// or a UI session.
func TestWorkspacesPage(t *testing.T) {
	env := startUIEnv(t, "")
	env.addGrantTemplates()

	t.Run("the page shows the defaults, lists and open verified workspaces", func(t *testing.T) {
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
		assert.Contains(t, page, "Choose which of your servers Claude Code can use on each machine.")
		assert.Contains(t, page, "Machines without their own choice")
		assert.Contains(t, page, "Other machines, such as your laptop")
		assert.Contains(t, page, "coder/dave/blog")
		assert.Contains(t, page, "No servers (default)")
		assert.Contains(t, page, "Choose servers")
		assert.NotContains(t, page, "claimed/elsewhere", "only verified workspaces are offered")
		assert.NotContains(t, page, "Sign in again", "the page asks for a sign-in only when a change needs one")

		for _, word := range []string{"attested", "principal", "Clear the list", "Add a list", "Workspace lists"} {
			assert.NotContains(t, page, word)
		}
	})

	t.Run("a workspace with servers shows them as chips and offers the default", func(t *testing.T) {
		browser := env.insertSession(t, "ws-card", []string{"dev"})
		env.freshSignIn(t, "ws-card")

		response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/servers", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-card")}, "workspace": {"coder/dave/card"}, "server": {"optin-server", "any-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		servers, exists := env.listOf(t, "dave", "coder/dave/card")
		require.True(t, exists)
		assert.Equal(t, []string{"any-server", "optin-server"}, servers)

		page := readBody(t, get(t, browser, env.server.URL+ui.WorkspacesPath))
		assert.Contains(t, page, "These servers:")
		assert.Contains(t, page, `<span class="chip font-mono">optin-server</span>`)
		assert.Contains(t, page, "Go back to the default for this workspace?")
		assert.Contains(t, page, `value="optin-server" checked`)

		// Unchecking one server removes just it.
		response = post(t, browser, env.server.URL+ui.WorkspacesPath+"/servers", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-card")}, "workspace": {"coder/dave/card"}, "server": {"any-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		servers, _ = env.listOf(t, "dave", "coder/dave/card")
		assert.Equal(t, []string{"any-server"}, servers)

		response = post(t, browser, env.server.URL+ui.WorkspacesPath+"/clear", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-card")}, "workspace": {"coder/dave/card"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		_, exists = env.listOf(t, "dave", "coder/dave/card")
		assert.False(t, exists)
	})

	t.Run("a save that needs a sign-in comes back as a change to confirm", func(t *testing.T) {
		browser := env.insertSession(t, "ws-keep", []string{"dev"})
		env.freshSignIn(t, "ws-keep")
		env.ageSignIn(t, "ws-keep", ui.StepUpMaxAge+30*time.Second)

		response := post(t, browser, env.server.URL+ui.WorkspacesPath+"/servers", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-keep")}, "workspace": {"coder/dave/keep"}, "server": {"optin-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		stepUp := location(t, response)
		assert.Equal(t, ui.LoginPath, stepUp.Path)

		returnTo, err := url.Parse(stepUp.Query().Get("return_to"))
		require.NoError(t, err)
		assert.Equal(t, ui.WorkspacesPath, returnTo.Path)

		_, exists := env.listOf(t, "dave", "coder/dave/keep")
		assert.False(t, exists, "nothing changes before the user confirms")

		env.freshSignIn(t, "ws-keep")

		page := readBody(t, get(t, browser, env.server.URL+returnTo.RequestURI()))
		assert.Contains(t, page, "Confirm")
		assert.Contains(t, page, `action="/ui/workspaces/servers"`)
		assert.Contains(t, page, `name="server" value="optin-server"`)

		response = post(t, browser, env.server.URL+ui.WorkspacesPath+"/servers", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-keep")}, "workspace": {"coder/dave/keep"}, "server": {"optin-server"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)

		servers, _ := env.listOf(t, "dave", "coder/dave/keep")
		assert.Equal(t, []string{"optin-server"}, servers)
	})

	t.Run("a kept change with a stale sign-in asks to sign in again", func(t *testing.T) {
		browser := env.insertSession(t, "ws-keep-stale", []string{"dev"})
		env.freshSignIn(t, "ws-keep-stale")
		env.ageSignIn(t, "ws-keep-stale", ui.StepUpMaxAge+30*time.Second)

		page := readBody(t, get(t, browser, env.server.URL+ui.WorkspacesPath+"?pending=servers&workspace=coder%2Fdave%2Fx&server=any-server"))
		assert.Contains(t, page, "To make this change, sign in again.")
		assert.NotContains(t, page, "Confirm")
	})

	t.Run("an unknown kept action shows nothing", func(t *testing.T) {
		browser := env.insertSession(t, "ws-keep-bad", []string{"dev"})
		env.freshSignIn(t, "ws-keep-bad")

		page := readBody(t, get(t, browser, env.server.URL+ui.WorkspacesPath+"?pending=../../x&workspace=w"))
		assert.NotContains(t, page, "Confirm")
	})

	t.Run("a widening POST without a fresh step-up is refused", func(t *testing.T) {
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
		assert.Contains(t, stepUp.Query().Get("return_to"), ui.WorkspacesPath+"?pending=allow")

		_, exists := env.listOf(t, "dave", workspaceName)
		assert.False(t, exists, "the stale widening POST changes nothing")

		response = post(t, browser, env.server.URL+ui.WorkspacesPath+"/defaults", url.Values{
			ui.CSRFField: {csrfTokenOf("ws-stale")}, "verified": {"all"}, "unverified": {"none"},
		})
		require.Equal(t, http.StatusSeeOther, response.StatusCode)
		assert.Equal(t, ui.LoginPath, location(t, response).Path)
		assert.Equal(t, gateway.ScopeNone, env.scopesOf(t, "dave").VerifiedDefault)
	})

	t.Run("a widening POST with a fresh step-up applies", func(t *testing.T) {
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

	t.Run("a narrowing POST applies without a step-up", func(t *testing.T) {
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

	t.Run("a POST without the CSRF token is refused", func(t *testing.T) {
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

	t.Run("a request without a UI session is refused", func(t *testing.T) {
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

// A change request's page shows the change before and after, only
// to its owner, and approving it with a fresh step-up applies the change.
func TestChangeApprovalPage(t *testing.T) {
	env := startUIEnv(t, "admins")
	env.addGrantTemplates()

	t.Run("another user, admins too, gets 404", func(t *testing.T) {
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

	t.Run("approving applies the change; then the page offers nothing", func(t *testing.T) {
		browser := env.insertSession(t, "change-owner", []string{"dev"})
		env.freshSignIn(t, "change-owner")

		before := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/blog", Servers: []string{"any-server"}})
		require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/blog", Servers: []string{"optin-server"}}, gateway.ViaUI))

		response := get(t, browser, env.server.URL+ui.ApprovalPath(before))
		require.Equal(t, http.StatusOK, response.StatusCode)

		page := readBody(t, response)
		assert.Contains(t, page, `<td id="change-before" data-label="Before"><ul><li>optin-server</li></ul></td>`)
		assert.Contains(t, page, `<td id="change-after" data-label="After"><ul><li>any-server</li><li>optin-server</li></ul></td>`)
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

	t.Run("approving needs a fresh step-up", func(t *testing.T) {
		id := env.requestChange(t, gateway.WorkspaceChange{Action: gateway.WorkspaceDefaults, Verified: gateway.ScopeAll})

		browser := env.insertSession(t, "change-stale", []string{"dev"})
		env.freshSignIn(t, "change-stale")
		env.ageSignIn(t, "change-stale", ui.StepUpMaxAge+30*time.Second)

		page := readBody(t, get(t, browser, env.server.URL+ui.ApprovalPath(id)))
		assert.Contains(t, page, "Sign in again to continue")
		assert.Contains(t, page, `<td data-label="Before">none</td><td data-label="After">all</td>`, "the defaults change shows before and after")

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

	t.Run("the approval page needs a UI session", func(t *testing.T) {
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
