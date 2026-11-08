//go:build integration

package ui_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/ui"
)

func TestServersPageShowsScopeEnvironmentsAndKind(t *testing.T) {
	env := startUIEnv(t, "admins")
	browser := env.insertSession(t, "catalog-session", []string{"dev"})

	response := get(t, browser, env.server.URL+ui.HomePath)
	require.Equal(t, http.StatusOK, response.StatusCode)

	body := readBody(t, response)

	assert.Contains(t, body, `<span class="badge badge-neutral" title="All your sessions share one instance.">per-user<span class="sr-only">: All your sessions share one instance.</span></span>`)
	assert.Contains(t, body, `<span class="badge badge-neutral" title="Everyone shares one instance.">shared<span class="sr-only">: Everyone shares one instance.</span></span>`)
	assert.Contains(t, body, `<span class="badge badge-ok" title="Works from any signed-in machine.">any<span class="sr-only">: Works from any signed-in machine.</span></span>`)
	assert.Contains(t, body, `<span class="badge badge-warn" title="Needs an attested workspace.">attested<span class="sr-only">: Needs an attested workspace.</span></span>`)
	assert.Contains(t, body, `data-label="Tools">12</td>`, "the tool count")
	assert.NotContains(t, body, "What the words mean", "the glossary card is gone")
	assert.Contains(t, body, "What do these mean?")
	assert.NotContains(t, body, `<th scope="col">Reason</th>`, "no instance has a reason")
	assert.NotContains(t, body, `<th scope="col">Ready since</th>`, "no instance has a ready time")
	assert.NotContains(t, body, `<th scope="col">Instances</th>`, "the instance count column is for admins")
}

func TestServersPageShowsWorkspaceAvailability(t *testing.T) {
	tests := []struct {
		name     string
		changes  []gateway.WorkspaceChange
		contains []string
	}{
		{
			name: "everything blocked by default",
			contains: []string{
				`Verified workspaces: <span class="badge badge-bad">blocked</span>`,
				`Other machines: <span class="badge badge-bad">blocked</span>`,
			},
		},
		{
			name: "defaults allow all",
			changes: []gateway.WorkspaceChange{
				{Action: gateway.WorkspaceDefaults, Verified: gateway.ScopeAll, Unverified: gateway.ScopeAll},
			},
			contains: []string{
				`Verified workspaces: <span class="badge badge-ok">allowed</span>`,
				`Other machines: <span class="badge badge-ok">allowed</span>`,
			},
		},
		{
			name: "a workspace list and the unverified list",
			changes: []gateway.WorkspaceChange{
				{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/mail", Servers: []string{"any-server"}},
				{Action: gateway.WorkspaceAllow, Workspace: "coder/dave/blog", Servers: []string{"optin-server"}},
				{Action: gateway.WorkspaceDefaults, Unverified: gateway.ScopeList},
				{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: []string{"any-server"}},
			},
			contains: []string{
				`<span title="coder/dave/mail">Coder workspace mail</span>: <span class="badge badge-ok">allowed</span>`,
				`<span title="coder/dave/blog">Coder workspace blog</span>: <span class="badge badge-bad">blocked</span>`,
				`Other machines: <span class="badge badge-ok">allowed</span>`,
				`href="/ui/workspaces"`,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := startUIEnv(t, "admins")
			env.addGrantTemplates()
			browser := env.insertSession(t, "availability-session", []string{"dev"})

			for _, change := range test.changes {
				require.NoError(t, env.workspaces.Apply(t.Context(), daveIdentity(), change, gateway.ViaUI))
			}

			response := get(t, browser, env.server.URL+"/ui/servers/any-server")
			require.Equal(t, http.StatusOK, response.StatusCode)

			body := readBody(t, response)
			for _, want := range test.contains {
				assert.Contains(t, body, want)
			}
		})
	}
}

func TestServerDetail(t *testing.T) {
	env := startUIEnv(t, "admins")
	browser := env.insertSession(t, "detail-session", []string{"dev"})

	t.Run("tools and commands", func(t *testing.T) {
		response := get(t, browser, env.server.URL+"/ui/servers/github")
		require.Equal(t, http.StatusOK, response.StatusCode)

		body := readBody(t, response)
		assert.Contains(t, body, "<code>create_issue</code>")
		assert.Contains(t, body, `<span class="badge badge-bad">blocked</span>`)
		assert.Contains(t, body, `<span class="badge badge-warn">needs approval</span>`)
		assert.Contains(t, body, "toolgate workspace allow github")
		assert.Contains(t, body, `<span class="chip"><span class="chip-label">group</span> <span class="font-mono">dev-*</span></span>`, "the access patterns show as chips")
		assert.Contains(t, body, `<span class="chip"><span class="chip-label">users</span> <span class="font-mono">*@example.com</span></span>`)
	})

	t.Run("a remote server shows only whether its secret is set", func(t *testing.T) {
		response := get(t, browser, env.server.URL+"/ui/servers/mail")
		require.Equal(t, http.StatusOK, response.StatusCode)

		body := readBody(t, response)
		assert.Contains(t, body, "Secret <code>mail-archiver-api-key</code></dt><dd class=\"m-0\">not set</dd>")
		assert.Contains(t, body, "<dt class=\"text-muted\">Kind</dt><dd class=\"m-0\">remote</dd>")
		assert.Contains(t, body, "<dt class=\"text-muted\">Access</dt><dd class=\"m-0\"><span class=\"inline-flex flex-wrap gap-1\">Only the users and groups the admin named</span></dd>")
	})

	t.Run("a server the user can't see is not found", func(t *testing.T) {
		response := get(t, browser, env.server.URL+"/ui/servers/hidden")
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
		assert.NotContains(t, readBody(t, response), "hidden")
	})
}
