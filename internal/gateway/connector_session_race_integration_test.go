//go:build integration

package gateway_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// runningInstances counts the instances of an mcp. session's per-session
// scope that are meant to run.
func (f *mcpFixture) runningInstances(t *testing.T, session string) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM instances WHERE scope_key = $1 AND desired = 'running'", "session:alice:"+session).Scan(&count))

	return count
}

func TestConnectorSessionEndRaces(t *testing.T) {
	t.Run("row 225: a request racing the DELETE of the MCP session doesn't resume the ended mcp. session", func(t *testing.T) {
		fixture := newMCPFixture(t)

		ended := make(chan struct{})
		proceed := make(chan struct{})

		// Hold the DELETE after its end transaction commits, while its MCP
		// session is still bound, so a second request passes the binding
		// check and finds the session ended.
		gateway.WrapEndSession(fixture.handlers[0], func(next func(context.Context, int64, string) error) func(context.Context, int64, string) error {
			return func(ctx context.Context, id int64, reason string) error {
				err := next(ctx, id, reason)

				close(ended)
				<-proceed

				return err
			}
		})

		fixture.addEnableTemplate("github", "per-session", anyone)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())
		session := auth.MCPSessionPrefix + mcpSessionID

		require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
			SessionPk: fixture.sessionRow(t, "alice", session).ID, TemplateNs: templateNamespace, TemplateName: "github",
		}))

		deleted := make(chan int, 1)
		go func() { deleted <- fixture.deleteMCPSession(t, connectorClient(), mcpSessionID) }()

		select {
		case <-ended:
		case <-time.After(30 * time.Second):
			t.Fatal("the DELETE didn't end the session")
		}

		require.True(t, fixture.sessionRow(t, "alice", session).EndedAt.Valid, "the DELETE ended the session")

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, connectorClient(), mcpSessionID))

		close(proceed)
		require.Equal(t, http.StatusNoContent, <-deleted)

		assert.True(t, fixture.sessionRow(t, "alice", session).EndedAt.Valid, "the session stays ended")
		assert.Empty(t, fixture.auditRows(t, "session.resume"))
		assert.Zero(t, fixture.runningInstances(t, session), "its per-session instances stop")
	})

	t.Run("row 225: a request racing the idle release of the MCP session doesn't resume the ended mcp. session", func(t *testing.T) {
		const idleTimeout = 500 * time.Millisecond

		fixture := newMCPFixture(t, withMCPSessionIdleTimeout(idleTimeout))
		fixture.addEnableTemplate("github", "per-session", anyone)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())
		session := auth.MCPSessionPrefix + mcpSessionID
		row := fixture.sessionRow(t, "alice", session)

		require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
			SessionPk: row.ID, TemplateNs: templateNamespace, TemplateName: "github",
		}))

		require.Eventually(t, func() bool {
			return fixture.sessionRow(t, "alice", session).EndedAt.Valid
		}, 30*idleTimeout, idleTimeout/5)

		// A request that passed the binding check before the release looks
		// its session up after the end committed.
		_, err := gateway.ResolveSession(t.Context(), fixture.handlers[0], auth.Identity{
			Principal: "alice", Session: session, Workspace: row.Workspace, Attestation: row.Attestation, Kind: row.Kind,
		})
		require.Error(t, err, "the lookup refuses the ended session")

		assert.True(t, fixture.sessionRow(t, "alice", session).EndedAt.Valid, "the session stays ended")
		assert.Empty(t, fixture.auditRows(t, "session.resume"))
		assert.Zero(t, fixture.runningInstances(t, session))
	})
}
