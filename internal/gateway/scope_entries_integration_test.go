//go:build integration

package gateway_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// userScopeID returns principal's user_scopes ID, creating the row.
func (f *mcpFixture) userScopeID(t *testing.T, principal string) int64 {
	t.Helper()

	require.NoError(t, store.EnsureUserScope(t.Context(), f.pool, principal))

	var id int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT id FROM user_scopes WHERE principal = $1`, principal).Scan(&id))

	return id
}

// addWorkspaceEntry inserts a scope entry of alice's for workspace.
func (f *mcpFixture) addWorkspaceEntry(t *testing.T, workspace, server, tool, effect string) {
	t.Helper()

	_, err := db.New(f.pool).InsertScopeEntry(t.Context(), db.InsertScopeEntryParams{
		ScopeID: f.userScopeID(t, "alice"), Workspace: pgtype.Text{String: workspace, Valid: true},
		Server: server, Tool: tool, Effect: effect,
	})
	require.NoError(t, err)
}

// addSessionEntry inserts a scope entry of alice's for her session.
func (f *mcpFixture) addSessionEntry(t *testing.T, session, server, tool, effect string) {
	t.Helper()

	_, err := db.New(f.pool).InsertScopeEntry(t.Context(), db.InsertScopeEntryParams{
		ScopeID: f.userScopeID(t, "alice"), SessionPk: pgtype.Int8{Int64: f.sessionRow(t, "alice", session).ID, Valid: true},
		Server: server, Tool: tool, Effect: effect,
	})
	require.NoError(t, err)
}

func (f *mcpFixture) scopeEntryCount(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM scope_entries`).Scan(&count))

	return count
}

// setVerifiedDefault writes alice's verified default.
func (f *mcpFixture) setVerifiedDefault(t *testing.T, value string) {
	t.Helper()

	f.userScopeID(t, "alice")
	_, err := f.pool.Exec(t.Context(), `UPDATE user_scopes SET verified_default = $1 WHERE principal = 'alice'`, value)
	require.NoError(t, err)
}

// setUnverifiedList stores alice's unverified default list with servers.
func (f *mcpFixture) setUnverifiedList(t *testing.T, servers ...string) {
	t.Helper()

	id := f.userScopeID(t, "alice")
	queries := db.New(f.pool)
	_, err := f.pool.Exec(t.Context(), `UPDATE user_scopes SET verified_default = 'none', unverified_default = 'none' WHERE id = $1`, id)
	require.NoError(t, err)
	require.NoError(t, queries.SetUserScopeUnverifiedList(t.Context(), db.SetUserScopeUnverifiedListParams{ID: id, UnverifiedList: true}))

	if len(servers) > 0 {
		_, err := queries.AddUnverifiedServers(t.Context(), db.AddUnverifiedServersParams{ScopeID: id, Servers: servers})
		require.NoError(t, err)
	}
}

func assertDenied(t *testing.T, entries []gateway.ServerEntry, name, reason string) {
	t.Helper()

	entry, ok := entryNamed(entries, name)
	require.True(t, ok, "%s is listed", name)
	assert.Equal(t, "blocked", entry.Status)
	assert.Equal(t, reason, entry.Reason)
}

// assertDeniedEnable checks that enabling github was refused at once by a
// deny entry: the reason, no request and server.enable with reason denied.
func assertDeniedEnable(t *testing.T, fixture *mcpFixture, result string, message string) {
	t.Helper()

	assert.Equal(t, message, result)
	assert.Zero(t, fixture.accessRequestCount(t), "a deny is never asked about")
	assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))

	enables := fixture.auditRows(t, "server.enable")
	require.Len(t, enables, 1)
	assert.Equal(t, "refuse", enables[0].Details["decision"])
	assert.Equal(t, policy.StepWorkspaceScope, enables[0].Details["step"])
	assert.Equal(t, policy.ReasonDenied, enables[0].Details["reason"])
}

func TestScopeEntries(t *testing.T) {
	t.Run("row 236: a workspace deny refuses an enable at once and list_servers shows it denied in this workspace", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "", policy.EffectDeny)

		client := fixture.connectElicitingAs(t, attestedAlice(), elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		assertDenied(t, listServers(t, client.session), "github", "denied in this workspace")

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assertDeniedEnable(t, fixture, resultText(result), "`github` is denied in workspace `"+aliceWorkspace+"`.")
		assert.Empty(t, client.elicitations(), "no elicitation")
	})

	t.Run("row 236: a session deny refuses an enable at once and list_servers shows it denied in this session", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)

		client := fixture.connectObserved(t, attestedAlice())
		fixture.addSessionEntry(t, "session-1", "github", "", policy.EffectDeny)

		assertDenied(t, listServers(t, client.session), "github", "denied in this session")

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assertDeniedEnable(t, fixture, resultText(result), "`github` is denied in this session.")
	})

	t.Run("an unverified session applies its session's deny entries", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		client := fixture.connectObserved(t, alice())
		fixture.addSessionEntry(t, "session-1", "github", "", policy.EffectDeny)

		assertDenied(t, listServers(t, client.session), "github", "denied in this session")

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assertDeniedEnable(t, fixture, resultText(result), "`github` is denied in this session.")
	})

	t.Run("an unverified session ignores its session's allow entries", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)

		client := fixture.connectObserved(t, alice())
		fixture.addSessionEntry(t, "session-1", "github", "echo", policy.EffectAllow)
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultNone)

		assert.Empty(t, listServers(t, client.session))
	})

	t.Run("a tool deny withdraws only that tool, and an allow entry lists only its tool", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		fixture.addEnableTemplate("k8s", "per-user", aliceAndBob)
		fixture.startUpstream(t, "k8s")
		fixture.setVerifiedDefault(t, policy.ScopeDefaultNone)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "k8s", "get_pods", policy.EffectAllow)

		client := fixture.connectObserved(t, attestedAlice())
		assertNotBlocked(t, listServers(t, client.session), "k8s")
		assertBlockedInWorkspace(t, listServers(t, client.session), "github")

		fixture.enableWithTools(t, client, "k8s", "user:alice", "get_pods", "delete_pod")
		assert.ElementsMatch(t, append(metaToolNames, "k8s__get_pods"), toolNames(t, client.session))

		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "k8s", "delete_pod", policy.EffectDeny)
		assert.ElementsMatch(t, append(metaToolNames, "k8s__get_pods"), toolNames(t, client.session))

		result := callTool(t, client.session, "k8s__delete_pod", map[string]any{})
		require.True(t, result.IsError)
		assert.Contains(t, resultText(result), "`k8s__delete_pod` is denied in workspace `"+aliceWorkspace+"`.")

		decisions := fixture.auditRows(t, "tool.decision")
		require.NotEmpty(t, decisions)
		last := decisions[len(decisions)-1]
		assert.Equal(t, policy.StepWorkspaceScope, last.Details["step"])
		assert.Equal(t, policy.ReasonDenied, last.Details["reason"])
	})

	t.Run("row 237: a server approval deletes no deny entry, and the wait ends approved, but still refused", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		client := fixture.connectElicitingAs(t, attestedAlice(), elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		results := startEnable(t, client.session, "github")
		fixture.awaitWaiting(t, client)

		request := fixture.awaitServerRequests(t, 1)[0]
		fixture.addSessionEntry(t, "session-1", "github", "", policy.EffectDeny)
		require.NoError(t, approveServerRequest(t, fixture, request.ID, gateway.AdditionSession))

		outcome := awaitEnable(t, results)
		assert.Equal(t, "`github` is denied in this session.", outcome.err)
		assert.Equal(t, 1, fixture.scopeEntryCount(t), "the approval deletes no deny entry")
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))

		enables := fixture.auditRows(t, "server.enable")
		require.Len(t, enables, 1)
		assert.Equal(t, "refuse", enables[0].Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, enables[0].Details["step"])
		assert.Equal(t, policy.ReasonDenied, enables[0].Details["reason"])
		assert.Equal(t, request.ID, enables[0].Details["request"])
	})

	t.Run("row 254: without the web UI deny entries do nothing", func(t *testing.T) {
		fixture := newMCPFixtureWithAuth(t, attestedUsers())
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.startUpstream(t, "github")
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "", policy.EffectDeny)

		client := fixture.connectObserved(t, attestedAlice())
		fixture.addSessionEntry(t, "session-1", "github", "echo", policy.EffectDeny)

		assertNotBlocked(t, listServers(t, client.session), "github")
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))
	})
}

func TestUnverifiedAllowList(t *testing.T) {
	t.Run("list gives the user's unverified servers", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.addEnableTemplate("slack", "per-user", aliceAndBob)
		fixture.setUnverifiedList(t, "github")

		client := fixture.connectObserved(t, alice())
		assert.Equal(t, []string{"github"}, serverNames(listServers(t, client.session)))

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "slack"})
		require.True(t, result.IsError)
		assert.Equal(t, unverifiedRefusal("slack"), resultText(result))
	})

	t.Run("list with no servers gives nothing", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.setUnverifiedList(t)

		client := fixture.connectObserved(t, alice())
		assert.Empty(t, listServers(t, client.session))
	})

	t.Run("a verified workspace ignores the unverified list", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		fixture.setUnverifiedList(t, "github")

		client := fixture.connectObserved(t, attestedAlice())
		assertBlockedInWorkspace(t, listServers(t, client.session), "github")
	})
}
