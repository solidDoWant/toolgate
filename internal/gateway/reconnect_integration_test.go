//go:build integration

package gateway_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enabledIn returns the names of the servers list_servers shows as enabled.
func enabledIn(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	var names []string

	for _, server := range listServers(t, session) {
		if server.Enabled {
			names = append(names, server.Name)
		}
	}

	return names
}

func TestReconnectKeepsTheSession(t *testing.T) {
	t.Run("a client reconnecting with the same session ID finds its servers enabled and listed", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)

		first := fixture.connectObserved(t, alice())
		fixture.enableReady(t, first, "github", "user:alice")
		require.NoError(t, first.session.Close())

		again := fixture.connectObserved(t, alice())
		require.NotEqual(t, first.session.ID(), again.session.ID(), "the reconnect is a new MCP session")

		assert.Equal(t, []string{"github"}, enabledIn(t, again.session))
		assert.ElementsMatch(t, append(metaToolNames, "github__search"), toolNames(t, again.session))
		assert.Len(t, fixture.auditEvents(t, "server.enable", "github"), 1, "the reconnect enabled nothing again")
	})
}

func TestReplicaLossKeepsTheSession(t *testing.T) {
	t.Run("when the replica holding the stream goes away, the client continues on another with no duplicate instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		github := fixture.addEnableTemplate("github", "per-user", anyone)
		other := fixture.replica(t)

		// The enable is waiting on the first replica when it goes away.
		first := fixture.connectObserved(t, alice())
		oldMCPSession := first.session.ID()
		waiting := startEnable(t, first.session, "github")
		instance := fixture.awaitInstance(t, github, "user:alice")

		fixture.server.CloseClientConnections()
		fixture.server.Close()
		assert.NotEmpty(t, awaitEnable(t, waiting).err, "the enable on the lost replica fails")

		// The other replica doesn't know the old MCP session: 404, so the
		// client initializes again.
		listTools := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}
		fixture.server = other
		require.Equal(t, http.StatusNotFound, fixture.post(t, alice(), oldMCPSession, listTools).StatusCode)

		again := fixture.connectObservedTo(t, other.URL, alice())
		assert.Equal(t, []string{"github"}, enabledIn(t, again.session), "the session's servers stay enabled")
		assert.ElementsMatch(t, metaToolNames, toolNames(t, again.session), "no tools until the instance is ready")

		// Enabling again waits on the same instance.
		results := startEnable(t, again.session, "github")
		require.Eventually(t, func() bool {
			return len(again.progressMessages()) > 0
		}, waitTimeout, 10*time.Millisecond, "the second enable should wait on the instance")

		fixture.markReady(t, instance.ID, "search")
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, results))

		instances := fixture.instancesOf(t, github)
		require.Len(t, instances, 1, "no duplicate instance")
		assert.Equal(t, instance.ID, instances[0].ID)
		assert.ElementsMatch(t, append(metaToolNames, "github__search"), toolNames(t, again.session))
	})
}
