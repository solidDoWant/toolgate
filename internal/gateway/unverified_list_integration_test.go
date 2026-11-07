//go:build integration

package gateway_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// unverifiedAlice is alice without a verified workspace.
func unverifiedAlice() user {
	client := alice()
	client.session = "unverified-1"

	return client
}

// unverifiedChange is an allow or a deny of the unverified allow list.
func unverifiedChange(action string, servers ...string) workspacesapi.Change {
	return workspacesapi.Change{Action: action, Unverified: workspacesapi.Unverified{List: true}, Servers: servers}
}

// unverifiedDefaults is a defaults change of the unverified default.
func unverifiedDefaults(value string) workspacesapi.Change {
	return workspacesapi.Change{Action: "defaults", Unverified: workspacesapi.Unverified{Default: value}}
}

// A user sets an allow list for their unverified sessions. Such a
// session lists and can enable only those servers. Adding a server needs an
// approved request; removing one withdraws it within 5 s.
func TestUnverifiedAllowListInGatewayRequests(t *testing.T) {
	t.Run("list with two servers: an unverified session lists and enables only those", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		for _, name := range []string{"github", "gitlab", "jira"} {
			fixture.addEnableTemplate(name, "per-user", aliceAndBob)
		}

		fixture.startUpstream(t, "github")

		// none to list with an empty list narrows nothing: it applies at once.
		api.applyChange(t, unverifiedAlice(), unverifiedDefaults("list"))

		id := api.requestChange(t, unverifiedAlice(), unverifiedChange("allow", "github", "gitlab"))
		kind, status, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "scope", kind)
		assert.Equal(t, "pending", status)
		assert.Empty(t, api.list(t, unverifiedAlice()).Defaults.UnverifiedServers, "nothing changes before the approval")

		require.NoError(t, api.approveScope(t, aliceIdentity(), id))

		defaults := api.list(t, unverifiedAlice()).Defaults
		assert.Equal(t, workspacesapi.Defaults{Verified: "none", Unverified: "list", UnverifiedServers: []string{"github", "gitlab"}}, defaults)

		client := fixture.connectObserved(t, unverifiedAlice())
		assert.Equal(t, []string{"github", "gitlab"}, serverNames(listServers(t, client.session)))

		refused := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "jira"})
		require.True(t, refused.IsError)
		assert.Contains(t, resultText(refused), "jira")

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
	})

	t.Run("adding a server needs an approved request, and an open session sees it within 5 s", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		for _, name := range []string{"github", "jira"} {
			fixture.addEnableTemplate(name, "per-user", aliceAndBob)
		}

		api.applyChange(t, unverifiedAlice(), unverifiedDefaults("list"))
		require.NoError(t, api.workspaces.Apply(t.Context(), aliceIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: []string{"github"}}, gateway.ViaUI))

		client := fixture.connectObserved(t, unverifiedAlice())
		assert.Equal(t, []string{"github"}, serverNames(listServers(t, client.session)))

		id := api.requestChange(t, unverifiedAlice(), unverifiedChange("allow", "jira"))
		assert.Equal(t, []string{"github"}, serverNames(listServers(t, client.session)), "nothing changes before the approval")

		require.NoError(t, api.approveScope(t, aliceIdentity(), id))
		require.Eventually(t, func() bool {
			return slices.Equal([]string{"github", "jira"}, serverNames(listServers(t, client.session)))
		}, scopeChangeBound, 10*time.Millisecond)

		changes := fixture.auditRows(t, gateway.TypeWorkspaceChange)
		require.NotEmpty(t, changes)
		last := changes[len(changes)-1]
		assert.Equal(t, true, last.Details["unverified"])
		assert.Equal(t, []any{"jira"}, last.Details["added"])
		assert.Equal(t, id, last.Details["via"])
	})

	t.Run("removing a server withdraws it within 5 s, without a request", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.startUpstream(t, "github")

		api.applyChange(t, unverifiedAlice(), unverifiedDefaults("list"))
		require.NoError(t, api.workspaces.Apply(t.Context(), aliceIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: []string{"github"}}, gateway.ViaUI))

		client := fixture.connectObserved(t, unverifiedAlice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		before := client.listChangedCount()

		api.applyChange(t, unverifiedAlice(), unverifiedChange("deny", "github"))

		require.Eventually(t, func() bool { return client.listChangedCount() > before }, scopeChangeBound, 10*time.Millisecond)

		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		require.True(t, refused.IsError)
		assert.Equal(t, unverifiedRefusal("github"), resultText(refused))
		assert.Empty(t, api.list(t, unverifiedAlice()).Defaults.UnverifiedServers)
	})

	t.Run("none to list with servers and list to all widen; all to list narrows", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		require.NoError(t, api.workspaces.Apply(t.Context(), aliceIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: []string{"github"}}, gateway.ViaUI))

		id := api.requestChange(t, unverifiedAlice(), unverifiedDefaults("list"))
		kind, _, _ := fixture.accessRequestRow(t, id)
		assert.Equal(t, "scope", kind, "phase 3's change request can't hold list")
		assert.Equal(t, "none", api.list(t, unverifiedAlice()).Defaults.Unverified)

		require.NoError(t, api.approveScope(t, aliceIdentity(), id))
		assert.Equal(t, "list", api.list(t, unverifiedAlice()).Defaults.Unverified)

		// list is stored as none, so a move to all widens (row 241).
		id = api.requestChange(t, unverifiedAlice(), unverifiedDefaults("all"))
		kind, _, _ = fixture.accessRequestRow(t, id)
		assert.Equal(t, "change", kind)
		assert.Equal(t, "list", api.list(t, unverifiedAlice()).Defaults.Unverified)

		require.NoError(t, api.approve(t, id))
		assert.Equal(t, "all", api.list(t, unverifiedAlice()).Defaults.Unverified)

		api.applyChange(t, unverifiedAlice(), unverifiedDefaults("list"))
		assert.Equal(t, "list", api.list(t, unverifiedAlice()).Defaults.Unverified)

		defaults := fixture.auditRows(t, gateway.TypeWorkspaceDefaults)
		require.NotEmpty(t, defaults)
		last := defaults[len(defaults)-1]
		assert.Equal(t, map[string]any{"verified": "none", "unverified": "all"}, last.Details["old"])
		assert.Equal(t, map[string]any{"verified": "none", "unverified": "list"}, last.Details["new"])
		assert.Equal(t, []any{"github"}, last.Details["unverifiedServers"])
	})

	t.Run("the list holds at most 64 servers", func(t *testing.T) {
		fixture, api := newScopedFixture(t)

		servers := make([]string, 0, gateway.MaxUnverifiedServers+1)
		for index := range gateway.MaxUnverifiedServers + 1 {
			name := fmt.Sprintf("server-%02d", index)
			fixture.addEnableTemplate(name, "per-user", aliceAndBob)
			servers = append(servers, name)
		}

		require.NoError(t, api.workspaces.Apply(t.Context(), aliceIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: servers[:64]}, gateway.ViaUI))
		assert.Len(t, api.list(t, unverifiedAlice()).Defaults.UnverifiedServers, 64)

		err := api.workspaces.Apply(t.Context(), aliceIdentity(),
			gateway.WorkspaceChange{Action: gateway.WorkspaceAllow, UnverifiedList: true, Servers: servers[64:]}, gateway.ViaUI)

		var refusal *gateway.WorkspaceError
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, http.StatusConflict, refusal.Status)
		assert.Len(t, api.list(t, unverifiedAlice()).Defaults.UnverifiedServers, 64)
	})

	t.Run("an allow needs access to the server and names no tools", func(t *testing.T) {
		fixture, api := newScopedFixture(t)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		bobOnly := fixture.addEnableTemplate("bobs", "per-user", aliceAndBob)
		bobOnly.Spec.Access.Users = []string{"bob"}
		fixture.catalog.Set(bobOnly, nil)

		status, body := api.change(t, unverifiedAlice(), unverifiedChange("allow", "bobs"))
		assert.Equal(t, http.StatusForbidden, status, body)

		status, body = api.change(t, unverifiedAlice(), unverifiedChange("allow", "github__echo"))
		assert.Equal(t, http.StatusBadRequest, status, body)

		status, body = api.change(t, unverifiedAlice(), workspacesapi.Change{
			Action: "allow", Unverified: workspacesapi.Unverified{List: true}, Workspace: aliceWorkspace, Servers: []string{"github"},
		})
		assert.Equal(t, http.StatusBadRequest, status, body)
	})

	t.Run("row 241: a 0.3 write of none leaves unverified_list, so list stays list", func(t *testing.T) {
		fixture, api := newScopedFixture(t)

		api.applyChange(t, unverifiedAlice(), unverifiedDefaults("list"))

		// The defaults write of a 0.3 replica, which doesn't know
		// unverified_list.
		_, err := fixture.pool.Exec(t.Context(),
			`UPDATE user_scopes SET verified_default = $2, unverified_default = $3, updated_at = now() WHERE principal = $1`,
			"alice", "none", "none")
		require.NoError(t, err)
		assert.Equal(t, "list", api.list(t, unverifiedAlice()).Defaults.Unverified)

		// A 0.4 write of none turns the list off.
		require.NoError(t, api.workspaces.Apply(t.Context(), auth.Identity{Principal: "alice", Kind: auth.KindUser},
			gateway.WorkspaceChange{Action: gateway.WorkspaceDefaults, Unverified: gateway.ScopeNone}, gateway.ViaCLI))
		assert.Equal(t, "none", api.list(t, unverifiedAlice()).Defaults.Unverified)
	})
}
