//go:build integration

package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// A request on a connector's MCP session that arrives while its DELETE is
// ending the mcp. session restarts (resumes) the ended session, which is then
// bound to no MCP session and never ended at once again.
func TestBughuntConnectorDeleteRaceResumesMCPSession(t *testing.T) {
	fixture := newMCPFixture(t)

	ended := make(chan struct{})
	proceed := make(chan struct{})

	gateway.BughuntWrapEndSession(fixture.handlers[0], func(next func(context.Context, int64, string) error) func(context.Context, int64, string) error {
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

	<-ended
	require.True(t, fixture.sessionRow(t, "alice", session).EndedAt.Valid, "the DELETE ended the session")

	// A concurrent request of the same client on the same MCP session.
	status := fixture.pingStatus(t, connectorClient(), mcpSessionID)
	t.Logf("concurrent request status: %d", status)

	close(proceed)
	require.Equal(t, http.StatusNoContent, <-deleted)

	row := fixture.sessionRow(t, "alice", session)
	assert.True(t, row.EndedAt.Valid, "an mcp. session that ended with its MCP session must never be resumed (design §9)")
	var instances int
	require.NoError(t, fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM instances WHERE scope_key = $1 AND desired = 'running'", "session:alice:"+session).Scan(&instances))
	t.Logf("session.resume events: %d, running per-session instances of the revived session: %d", len(fixture.auditRows(t, "session.resume")), instances)
	t.Logf("session.start events: %d, session.end events: %d", len(fixture.auditRows(t, "session.start")), len(fixture.endedSessionEvents(t, session)))
}
