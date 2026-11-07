//go:build integration

package gateway_test

import (
	"io"
	"net/http"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// connectorClient is alice signed in through a client that sends no
// X-Toolgate-Session, such as a claude.ai connector.
func connectorClient() user {
	client := alice()
	client.session = ""

	return client
}

var mcpSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// sessionCount counts the sessions rows.
func (f *mcpFixture) sessionCount(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&count))

	return count
}

// deleteMCPSession sends the DELETE that ends an MCP session and returns
// the HTTP status.
func (f *mcpFixture) deleteMCPSession(t *testing.T, client user, mcpSessionID string) int {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, f.server.URL, nil)
	require.NoError(t, err)

	request.Header = f.headers(t, client)
	request.Header.Set("Mcp-Session-Id", mcpSessionID)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	return response.StatusCode
}

// endedSessionEvents returns the session.end events of session.
func (f *mcpFixture) endedSessionEvents(t *testing.T, session string) []auditRow {
	t.Helper()

	var events []auditRow

	for _, event := range f.auditRows(t, "session.end") {
		if event.Session == session {
			events = append(events, event)
		}
	}

	return events
}

func TestConnectorSessions(t *testing.T) {
	t.Run("an initialize without the session header makes an unverified mcp. session bound to the MCP session", func(t *testing.T) {
		fixture := newMCPFixture(t)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())
		assert.Regexp(t, mcpSessionIDPattern, mcpSessionID)

		row := fixture.sessionRow(t, "alice", auth.MCPSessionPrefix+mcpSessionID)
		assert.Equal(t, "claimed/default", row.Workspace)
		assert.Equal(t, "user", row.Attestation)
		assert.False(t, row.EndedAt.Valid)

		assert.Equal(t, http.StatusOK, fixture.pingStatus(t, connectorClient(), mcpSessionID))
		assert.Equal(t, 1, fixture.sessionCount(t), "later requests use the same session")
	})

	t.Run("the workspace header narrows the mcp. session's claimed workspace", func(t *testing.T) {
		fixture := newMCPFixture(t)

		client := connectorClient()
		client.workspace = "laptop"

		mcpSessionID := fixture.initializeRaw(t, client)
		assert.Equal(t, "claimed/laptop", fixture.sessionRow(t, "alice", auth.MCPSessionPrefix+mcpSessionID).Workspace)
	})

	t.Run("two clients get separate sessions with their own enabled sets", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addTemplate("github", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})

		first := fixture.connect(t, connectorClient())
		second := fixture.connect(t, connectorClient())

		require.NotEqual(t, first.ID(), second.ID())
		require.Equal(t, 2, fixture.sessionCount(t))

		firstRow := fixture.sessionRow(t, "alice", auth.MCPSessionPrefix+first.ID())
		require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
			SessionPk: firstRow.ID, TemplateNs: templateNamespace, TemplateName: "github",
		}))

		enabled := func(entries []gateway.ServerEntry) bool {
			require.Len(t, entries, 1)

			return entries[0].Enabled
		}

		assert.True(t, enabled(listServers(t, first)))
		assert.False(t, enabled(listServers(t, second)))

		disable(t, first, "github")
		assert.False(t, enabled(listServers(t, first)))
	})

	t.Run("a request without the session header or an MCP session that isn't an initialize gets 400 and makes no session", func(t *testing.T) {
		fixture := newMCPFixture(t)

		response := fixture.post(t, connectorClient(), "", pingMessage())
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
		assert.Equal(t, 0, fixture.sessionCount(t))
	})

	t.Run("an Mcp-Session-Id no replica stored gets 404 and makes no session", func(t *testing.T) {
		fixture := newMCPFixture(t)

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, connectorClient(), "0123456789abcdef0123456789abcdef"))
		assert.Equal(t, 0, fixture.sessionCount(t))
	})

	t.Run("a client can't pick another MCP session's session by sending its ID", func(t *testing.T) {
		fixture := newMCPFixture(t)

		helperMCPSessionID := fixture.initializeRaw(t, alice())
		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, connectorClient(), helperMCPSessionID),
			"a helper's MCP session isn't the connector's")

		connectorMCPSessionID := fixture.initializeRaw(t, connectorClient())
		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, alice(), connectorMCPSessionID),
			"a connector's MCP session isn't the helper's")

		bobClient := bob()
		bobClient.session = ""
		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, bobClient, connectorMCPSessionID),
			"another user can't use the MCP session")

		_, err := db.New(fixture.pool).GetSession(t.Context(), db.GetSessionParams{
			Principal: "bob", SessionID: auth.MCPSessionPrefix + connectorMCPSessionID,
		})
		require.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("a session header with the mcp. prefix gets 400", func(t *testing.T) {
		fixture := newMCPFixture(t)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())

		client := alice()
		client.session = auth.MCPSessionPrefix + mcpSessionID
		assert.Equal(t, http.StatusBadRequest, fixture.pingStatus(t, client, mcpSessionID))
		assert.Equal(t, http.StatusBadRequest, fixture.post(t, client, "", initializeMessage()).StatusCode)
		assert.Equal(t, 1, fixture.sessionCount(t))
	})

	t.Run("a DELETE of the MCP session ends the session at once with session.end", func(t *testing.T) {
		fixture := newMCPFixture(t)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())
		session := auth.MCPSessionPrefix + mcpSessionID

		require.Equal(t, http.StatusNoContent, fixture.deleteMCPSession(t, connectorClient(), mcpSessionID))

		assert.True(t, fixture.sessionRow(t, "alice", session).EndedAt.Valid)

		events := fixture.endedSessionEvents(t, session)
		require.Len(t, events, 1)
		assert.Equal(t, "alice", events[0].Principal)
		assert.Equal(t, "mcp-session-delete", events[0].Details["reason"])

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, connectorClient(), mcpSessionID), "it is never resumed")
		assert.True(t, fixture.sessionRow(t, "alice", session).EndedAt.Valid)
	})

	t.Run("a DELETE of a helper's MCP session leaves its session live", func(t *testing.T) {
		fixture := newMCPFixture(t)

		mcpSessionID := fixture.initializeRaw(t, alice())
		require.Equal(t, http.StatusNoContent, fixture.deleteMCPSession(t, alice(), mcpSessionID))

		assert.False(t, fixture.sessionRow(t, "alice", "session-1").EndedAt.Valid)
		assert.Empty(t, fixture.auditRows(t, "session.end"))
	})

	t.Run("the leader's idle end of the MCP session ends the session with session.end", func(t *testing.T) {
		fixture := newMCPFixture(t)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())
		session := auth.MCPSessionPrefix + mcpSessionID

		fixture.endIdleMCPSessions(t)

		events := fixture.endedSessionEvents(t, session)
		require.Len(t, events, 1)
		assert.Equal(t, "mcp-session-idle", events[0].Details["reason"])

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, connectorClient(), mcpSessionID))
	})
}
