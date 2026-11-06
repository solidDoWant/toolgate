//go:build integration

package gateway_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const toolListScopes = "toolgate_list_scopes"

// newBuiltinFixture is a gateway with the web UI: workspace scoping on and
// the built-in server in the catalog.
func newBuiltinFixture(t *testing.T) *mcpFixture {
	t.Helper()

	fixture := newAdditionFixture(t)
	fixture.catalog.SetBuiltin(gateway.BuiltinTemplate())

	return fixture
}

// insertSessions opens count sessions of principal, named `<prefix>-<n>`.
func (f *mcpFixture) insertSessions(t *testing.T, principal, prefix string, count int) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `INSERT INTO sessions (principal, session_id, workspace, attestation, kind)
		SELECT $1, $2 || '-' || n, 'claimed/x', 'user', 'user' FROM generate_series(1, $3::int) n`, principal, prefix, count)
	require.NoError(t, err)
}

// insertWorkspaceLists gives principal count empty workspace lists, named
// `<prefix>-<n>`.
func (f *mcpFixture) insertWorkspaceLists(t *testing.T, principal, prefix string, count int) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `WITH scope AS (
			INSERT INTO user_scopes (principal) VALUES ($1)
			ON CONFLICT (principal) DO UPDATE SET principal = EXCLUDED.principal RETURNING id)
		INSERT INTO workspace_lists (scope_id, workspace)
		SELECT scope.id, $2 || '-' || n FROM scope, generate_series(1, $3::int) n`, principal, prefix, count)
	require.NoError(t, err)
}

func listScopesResult(t *testing.T, text string) gateway.ListScopesResult {
	t.Helper()

	var result gateway.ListScopesResult
	require.NoError(t, json.Unmarshal([]byte(text), &result))

	return result
}

func TestBuiltinServer(t *testing.T) {
	t.Run("row 210: without the web UI the server isn't in the catalog and its tools are refused with not-offered", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := fixture.connectObserved(t, attestedAlice())

		_, listed := entryNamed(listServers(t, client.session), gateway.BuiltinServerName)
		assert.False(t, listed)

		result := callTool(t, client.session, toolListScopes, nil)
		require.True(t, result.IsError)
		assert.Equal(t, "Server `toolgate` has no tool `list_scopes`.", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, gateway.BuiltinServerName, decisions[0].Server)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, "not-offered", decisions[0].Details["reason"])
	})

	t.Run("row 208: a call where toolgate isn't allowed reaches the call path and writes tool.decision", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		// alice's unverified session: the default for unverified sessions
		// is none, so step 4 refuses every server.
		client := fixture.connectObserved(t, alice())

		result := callTool(t, client.session, toolListScopes, nil)
		require.True(t, result.IsError)
		assert.Contains(t, resultText(result), "Server `toolgate` isn't allowed in sessions without a verified workspace.")

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, gateway.BuiltinServerName, decisions[0].Server)
		assert.Equal(t, "list_scopes", decisions[0].Tool)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, decisions[0].Details["step"])
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
	})

	t.Run("a rule naming toolgate refuses its tools", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		require.NoError(t, fixture.rules.Set(blockServer(gateway.BuiltinServerName)))
		client := fixture.connectObserved(t, attestedAlice())

		result := callTool(t, client.session, toolListScopes, nil)
		require.True(t, result.IsError)

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, policy.StepRule, decisions[0].Details["step"])
	})

	t.Run("toolgate_list_scopes lists only the caller's own scope, tools by exposed name, through the full call path", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		fixture.curate(t, "github", func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "open_issue"}}
		})
		fixture.setUnverifiedList(t, "github")
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		fixture.fillList(t, "coder/alice/other", 1)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "create_issue", policy.EffectAllow)
		fixture.addWorkspaceEntry(t, aliceWorkspace, "github", "delete_repository", policy.EffectDeny)
		fixture.insertWorkspaceLists(t, "bob", "bob-workspace", 1)
		fixture.insertSessions(t, "bob", "bob-session", 1)

		client := fixture.connectObserved(t, attestedAlice())

		assertReady := func(entries []gateway.ServerEntry) {
			entry, listed := entryNamed(entries, gateway.BuiltinServerName)
			require.True(t, listed)
			assert.Equal(t, gateway.StatusReady, entry.Status)
		}
		assertReady(listServers(t, client.session))

		sessionPK := fixture.sessionRow(t, "alice", "session-1").ID
		fixture.addSessionAddition(t, sessionPK, "immich")
		fixture.addSessionEntry(t, "session-1", "github", "", policy.EffectDeny)

		result := callTool(t, client.session, toolListScopes, nil)
		require.False(t, result.IsError, resultText(result))

		scopes := listScopesResult(t, resultText(result))
		assert.Equal(t, gateway.CurrentScope{Session: "session-1", Workspace: aliceWorkspace, Verified: true}, scopes.Current)
		assert.Equal(t, gateway.ScopeDefaults{Verified: "all", Unverified: "list", UnverifiedServers: []string{"github"}}, scopes.Defaults)
		assert.Equal(t, []gateway.WorkspaceSummary{
			{Workspace: aliceWorkspace, Servers: []string{}, Tools: []string{"github__open_issue"}, Denied: []string{"github__delete_repository"}},
			{Workspace: "coder/alice/other", Servers: []string{"filler-1"}, Tools: []string{}, Denied: []string{}},
		}, scopes.Workspaces)
		require.Len(t, scopes.Sessions, 1, "bob's session isn't listed")
		assert.Equal(t, "session-1", scopes.Sessions[0].Session)
		assert.True(t, scopes.Sessions[0].Verified)
		assert.Equal(t, []string{"immich"}, scopes.Sessions[0].Allowed)
		assert.Equal(t, []string{"github"}, scopes.Sessions[0].Denied)
		assert.False(t, scopes.Truncated)

		// Enabled by its first call, with no instance, and still ready.
		assert.Equal(t, []string{gateway.BuiltinServerName}, fixture.enabledServers(t, "alice", "session-1"))
		assert.Empty(t, fixture.instancesOf(t, gateway.BuiltinTemplate()))
		assertReady(listServers(t, client.session))
		assert.Contains(t, toolNames(t, client.session), toolListScopes)

		status := callTool(t, client.session, gateway.ToolServerStatus, map[string]any{"name": gateway.BuiltinServerName})
		require.False(t, status.IsError, resultText(status))
		assert.Contains(t, resultText(status), `"status":"ready"`)

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "allow", decisions[0].Details["decision"])
		assert.Equal(t, "list_scopes", decisions[0].Tool)

		results := fixture.auditRows(t, "tool.result")
		require.Len(t, results, 1)
		assert.Equal(t, gateway.BuiltinServerName, results[0].Server)
		assert.Equal(t, "ok", results[0].Details["outcome"])
	})

	t.Run("toolgate_list_scopes shows at most 200 workspaces and 200 sessions", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			count     int
			truncated bool
		}{
			{name: "200 each", count: 200},
			{name: "201 each", count: 201, truncated: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newBuiltinFixture(t)
				fixture.insertWorkspaceLists(t, "alice", "list", test.count)
				fixture.insertSessions(t, "alice", "other", test.count)

				workspaces := gateway.NewWorkspaces(fixture.pool, fixture.catalog, nil, 0)
				summary, err := workspaces.ScopeSummary(t.Context(), "alice", time.Hour)
				require.NoError(t, err)

				assert.Len(t, summary.Workspaces, 200)
				assert.Len(t, summary.Sessions, 200)
				assert.Equal(t, test.truncated, summary.Truncated)
			})
		}
	})
}

func TestBuiltinServerSearchRows(t *testing.T) {
	t.Run("a new leader writes the built-in server's search rows and the cleanup keeps them", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		queries := db.New(fixture.pool)

		rebuilder := &runtime.SearchRebuilder{Pool: fixture.pool, Templates: fixture.catalog.LastValidByUID}
		require.NoError(t, rebuilder.Sync(t.Context()))

		uid := string(gateway.BuiltinTemplate().UID)
		rows, err := queries.ListToolSearchForTemplate(t.Context(), uid)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "list_scopes", rows[0].UpstreamName)
		assert.Equal(t, gateway.BuiltinServerName, rows[0].TemplateName)

		cleaner := &runtime.CatalogCleaner{Queries: queries, TemplateUIDs: fixture.catalog.UIDs}
		require.NoError(t, cleaner.Run(t.Context()))

		rows, err = queries.ListToolSearchForTemplate(t.Context(), uid)
		require.NoError(t, err)
		assert.Len(t, rows, 1)
	})
}
