//go:build integration

package gateway_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// unverifiedServerStatus calls toolgate_server_status and returns the raw result.
func unverifiedServerStatus(t *testing.T, client user, fixture *mcpFixture, name string) (gateway.ServerStatusResult, string, bool) {
	t.Helper()

	session := fixture.connect(t, client)
	result := callTool(t, session, gateway.ToolServerStatus, map[string]any{"name": name})

	var status gateway.ServerStatusResult

	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(encoded, &status))
	}

	return status, resultText(result), result.IsError
}

// An unverified session shows the servers its user's access lists allow, as
// a verified one does; the ones the workspace scope blocks are blocked with
// the cause and the fix. Nothing about what is allowed changes.
func TestUnverifiedSessionShowsBlockedServers(t *testing.T) {
	const blockedReason = "not allowed in sessions without a verified workspace; set the default for unverified sessions (Workspaces page, or `toolgate workspace defaults --unverified …`), or allow it with `toolgate workspace allow --unverified <server>`"

	setup := func(t *testing.T) *mcpFixture {
		t.Helper()

		fixture := newMCPFixtureWithAuth(t, attestedUsers(), withWorkspaceScoping, withApprovalURL)
		github := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.declareTools(t, github, declared("zebra_read", "zebra read"))
		fixture.addEnableTemplate("private", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})
		fixture.startUpstream(t, "github")
		fixture.setVerifiedDefault(t, "none")
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultNone)

		return fixture
	}

	blocked := func(t *testing.T, fixture *mcpFixture) {
		t.Helper()

		session := fixture.connect(t, searching(alice()))

		entries := listServers(t, session)
		require.Len(t, entries, 1, "only the server the access lists allow is listed")
		assert.Equal(t, "github", entries[0].Name)
		assert.Equal(t, gateway.StatusBlocked, entries[0].Status)
		assert.Equal(t, blockedReason, entries[0].Reason)

		status, _, failed := unverifiedServerStatus(t, searching(alice()), fixture, "github")
		require.False(t, failed)
		assert.Equal(t, gateway.StatusBlocked, status.Status)
		assert.Equal(t, blockedReason, status.Reason)

		_, message, failed := unverifiedServerStatus(t, searching(alice()), fixture, "private")
		require.True(t, failed)
		assert.Contains(t, message, "isn't in your catalog")

		found, _ := findTools(t, session, "zebra", 20)
		assert.Empty(t, found.Tools, "no tool name of a blocked server is shown")
		assert.Contains(t, found.Note, "blocked in sessions without a verified workspace")
		assert.Contains(t, found.Note, "toolgate workspace allow --unverified")

		refused := callTool(t, session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, refused.IsError)
		assert.Equal(t, unverifiedRefusal("github"), resultText(refused))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
	}

	t.Run("default none blocks the server in list, status, find and enable", func(t *testing.T) {
		blocked(t, setup(t))
	})

	t.Run("default list with an empty list blocks the server the same way", func(t *testing.T) {
		fixture := setup(t)
		fixture.setUnverifiedList(t)
		blocked(t, fixture)
	})

	t.Run("default list shows a listed server as usable and another as blocked", func(t *testing.T) {
		fixture := setup(t)
		other := fixture.addEnableTemplate("other", "per-user", anyone)
		fixture.declareTools(t, other, declared("zebra_other", "zebra other"))
		fixture.setUnverifiedList(t, "github")

		session := fixture.connect(t, searching(alice()))

		statuses := map[string]string{}
		for _, entry := range listServers(t, session) {
			statuses[entry.Name] = entry.Status
		}

		assert.Equal(t, gateway.StatusBlocked, statuses["other"])
		assert.NotEqual(t, gateway.StatusBlocked, statuses["github"])

		found, _ := findTools(t, session, "zebra", 20)
		assert.Equal(t, []string{"github__zebra_read"}, foundNames(found))
		assert.Empty(t, found.Note)

		refused := callTool(t, session, gateway.ToolEnableServer, map[string]any{"name": "other"})
		require.True(t, refused.IsError)
		assert.Equal(t, unverifiedRefusal("other"), resultText(refused))
	})

	t.Run("default all lists the server as usable and a verified workspace sees it blocked only by its own list", func(t *testing.T) {
		fixture := setup(t)
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		session := fixture.connect(t, searching(alice()))
		entries := listServers(t, session)
		require.Len(t, entries, 1)
		assert.NotEqual(t, gateway.StatusBlocked, entries[0].Status)

		found, _ := findTools(t, session, "zebra", 20)
		assert.Equal(t, []string{"github__zebra_read"}, foundNames(found))

		attested := attestedAlice()
		attested.session = "verified-1"
		verified := fixture.connect(t, attested)
		entries = listServers(t, verified)
		require.Len(t, entries, 1)
		assert.Equal(t, gateway.StatusBlocked, entries[0].Status)
		assert.Equal(t, "not allowed in this workspace; enable it to ask", entries[0].Reason)
	})
}
