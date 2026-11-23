//go:build integration

package gateway_test

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// giveSessionState gives principal's session an enabled server, a session
// addition and a session-scoped allow entry, as enables and approvals made
// in that session do.
func (f *mcpFixture) giveSessionState(t *testing.T, principal, session string) int64 {
	t.Helper()

	row := f.sessionRow(t, principal, session)

	require.NoError(t, db.New(f.pool).EnableServer(t.Context(), db.EnableServerParams{
		SessionPk: row.ID, TemplateNs: templateNamespace, TemplateName: "github",
	}))
	f.addSessionAddition(t, row.ID, "github")

	_, err := db.New(f.pool).InsertScopeEntry(t.Context(), db.InsertScopeEntryParams{
		ScopeID: f.userScopeID(t, principal), SessionPk: pgtype.Int8{Int64: row.ID, Valid: true},
		Server: "github", Tool: "echo", Effect: policy.EffectAllow,
	})
	require.NoError(t, err)

	return row.ID
}

// sessionAllowEntries counts the allow entries of the session row id.
func (f *mcpFixture) sessionAllowEntries(t *testing.T, id int64) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM scope_entries WHERE session_pk = $1 AND effect = 'allow'`, id).Scan(&count))

	return count
}

// sessionEventsOf returns the audit rows of eventType for principal's session.
func (f *mcpFixture) sessionEventsOf(t *testing.T, eventType, principal, session string) []auditRow {
	t.Helper()

	var matching []auditRow

	for _, row := range f.auditRows(t, eventType) {
		if row.Principal == principal && row.Session == session {
			matching = append(matching, row)
		}
	}

	return matching
}

// requireRebound checks that the session moved from previous to workspace
// with nothing of the old session left.
func (f *mcpFixture) requireRebound(t *testing.T, client user, id int64, previous, workspace, attestation string) {
	t.Helper()

	f.requireSession(t, client, workspace, attestation)

	ends := f.sessionEventsOf(t, "session.end", client.claims.Subject, client.session)
	require.Len(t, ends, 1)
	assert.Equal(t, "workspace-changed", ends[0].Details["reason"])
	assert.Equal(t, previous, ends[0].Workspace)

	starts := f.sessionEventsOf(t, "session.start", client.claims.Subject, client.session)
	require.Len(t, starts, 2)
	assert.Equal(t, workspace, starts[1].Workspace)
	assert.Equal(t, attestation, starts[1].Attestation)
	assert.Equal(t, previous, starts[1].Details["previousWorkspace"])

	assert.Empty(t, f.enabledServers(t, client.claims.Subject, client.session), "enabled servers don't carry over")
	assert.Empty(t, f.sessionAdditions(t, id), "session additions don't carry over")
	assert.Zero(t, f.sessionAllowEntries(t, id), "session approvals don't carry over")
}

func TestNewMCPSessionMovesToTheNewWorkspace(t *testing.T) {
	t.Parallel()

	fixture, _ := newKeyFixture(t)
	api := newKeyAPI(t, fixture, true)

	t.Run("an unverified session that reconnects with a newly approved key moves to the key's workspace", func(t *testing.T) {
		key := newWorkspaceKey(t)
		pending := api.requestKey(t, keyUser("rebind-up"), "dev", key.publicKey())

		client := keyUser("rebind-up")
		oldMCPSession := fixture.startSession(t, fixture.server.URL, client)
		fixture.requireSession(t, client, "claimed/default", auth.AttestationUser)
		id := fixture.giveSessionState(t, "rebind-up", client.session)

		require.NoError(t, api.approveKey(t, "rebind-up", pending.Request))

		attested := withKey(t, client, key)
		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, attested, oldMCPSession),
			"the running MCP session doesn't change workspace")

		newMCPSession := fixture.startSession(t, fixture.server.URL, attested)
		fixture.requireRebound(t, client, id, "claimed/default", "registered/rebind-up/dev", auth.AttestationAttested)
		assert.Equal(t, id, fixture.sessionRow(t, "rebind-up", client.session).ID, "the session ID stays")
		assert.Equal(t, http.StatusOK, fixture.pingStatus(t, withKey(t, client, key), newMCPSession))

		// The old MCP session ended with the old session. A request in the
		// old workspace that starts no new MCP session is refused.
		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, client, oldMCPSession))
		assert.Equal(t, http.StatusBadRequest, fixture.post(t, client, "", listToolsMessage()).StatusCode)
		fixture.requireSession(t, client, "registered/rebind-up/dev", auth.AttestationAttested)
	})

	t.Run("an attested session that reconnects after its key was revoked moves to the unverified workspace", func(t *testing.T) {
		key := newWorkspaceKey(t)
		api.activateKey(t, "rebind-down", "dev", key.publicKey())

		client := keyUser("rebind-down")
		oldMCPSession := fixture.startSession(t, fixture.server.URL, withKey(t, client, key))
		fixture.requireSession(t, client, "registered/rebind-down/dev", auth.AttestationAttested)
		id := fixture.giveSessionState(t, "rebind-down", client.session)

		_, err := api.workspaces.RevokeKey(t.Context(), keyIdentity("rebind-down"), "dev", gateway.ViaCLI)
		require.NoError(t, err)

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, withKey(t, client, key), oldMCPSession),
			"the running MCP session doesn't change workspace")

		fixture.startSession(t, fixture.server.URL, withKey(t, client, key))
		fixture.requireRebound(t, client, id, "registered/rebind-down/dev", "claimed/default", auth.AttestationUser)
	})
}

func listToolsMessage() map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}
}
