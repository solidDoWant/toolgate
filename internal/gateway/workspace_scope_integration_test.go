//go:build integration

package gateway_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
)

// scopeChangeBound is how soon after a scoping write open sessions must get
// list_changed.
const scopeChangeBound = 5 * time.Second

func withWorkspaceScoping(options *gateway.MCPOptions) {
	options.WorkspaceScoping = true
}

// unverifiedRefusal is the step-4 refusal of server in a session without a
// verified workspace.
func unverifiedRefusal(server string) string {
	return "Server `" + server + "` isn't allowed in sessions without a verified workspace. To allow all your servers in them, set the default for unverified sessions to all on the Workspaces page, or run `toolgate workspace defaults --unverified all`."
}

// setUnverifiedDefault writes principal's unverified default and notifies
// scope:<id> in the same transaction, as a scoping write does.
func (f *mcpFixture) setUnverifiedDefault(t *testing.T, principal, value string) {
	t.Helper()

	ctx := context.Background()

	require.NoError(t, store.EnsureUserScope(ctx, f.pool, principal))

	transaction, err := f.pool.Begin(ctx)
	require.NoError(t, err)

	defer func() { _ = transaction.Rollback(ctx) }()

	var id int64
	require.NoError(t, transaction.QueryRow(ctx, `UPDATE user_scopes SET unverified_default = $2, updated_at = now()
		WHERE principal = $1 RETURNING id`, principal, value).Scan(&id))
	require.NoError(t, store.Notify(ctx, transaction, store.KindScope, strconv.FormatInt(id, 10)))
	require.NoError(t, transaction.Commit(ctx))
}

func (f *mcpFixture) accessRequestCount(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM access_requests`).Scan(&count))

	return count
}

func serverNames(entries []gateway.ServerEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}

	return names
}

func TestWorkspaceScope(t *testing.T) {
	everyone := v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"}

	t.Run("an unverified session with no user_scopes row sees no servers, gets no tools and is refused an enable without a request", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", everyone)

		client := fixture.connectObserved(t, alice())

		assert.Empty(t, listServers(t, client.session))

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assert.Equal(t, unverifiedRefusal("github"), resultText(result))
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.Zero(t, fixture.accessRequestCount(t), "an unverified enable inserts no request")

		enables := fixture.auditRows(t, "server.enable")
		require.Len(t, enables, 1)
		assert.Equal(t, "refuse", enables[0].Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, enables[0].Details["step"])
	})

	t.Run("row 162: without the web UI the step is off and a user with no row sees every server", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", everyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())

		assert.Equal(t, []string{"github"}, serverNames(listServers(t, client.session)))
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
	})

	t.Run("row 122, row 123: narrowing the default on another replica withdraws the tools within 5 s and refuses calls; widening brings them back without an enable", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", everyone)
		fixture.startUpstream(t, "github")
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)
		fixture.setUnverifiedDefault(t, "bob", policy.ScopeDefaultAll)

		alices := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, alices, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return alices.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		bobs := fixture.connectObserved(t, bob())
		fixture.enableWithTools(t, bobs, "github", "user:bob", "echo")
		require.Eventually(t, func() bool { return bobs.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		// A second MCP session of alice's, on another replica, which only
		// the scope:<id> notification tells about the write.
		other := fixture.replica(t)
		elsewhere := fixture.connectObservedTo(t, other.URL, alice())
		require.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, elsewhere.session))

		started := time.Now()

		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultNone)

		require.Eventually(t, func() bool { return alices.listChangedCount() == 2 }, scopeChangeBound, 10*time.Millisecond,
			"alice's session got no list_changed within 5 s")
		require.Eventually(t, func() bool { return elsewhere.listChangedCount() >= 1 }, scopeChangeBound, 10*time.Millisecond,
			"alice's session on the other replica got no list_changed within 5 s")
		assert.Less(t, time.Since(started), scopeChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, elsewhere.session))
		assert.ElementsMatch(t, metaToolNames, toolNames(t, alices.session))
		assert.Empty(t, listServers(t, alices.session), "an unverified session leaves refused servers out")
		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"), "the server stays enabled")

		refused := callTool(t, alices.session, "github__echo", map[string]any{"text": "x"})
		require.True(t, refused.IsError)
		assert.Equal(t, unverifiedRefusal("github"), resultText(refused))

		decisions := fixture.auditRows(t, "tool.decision")
		require.NotEmpty(t, decisions)
		last := decisions[len(decisions)-1]
		assert.Equal(t, "refuse", last.Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, last.Details["step"])

		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, bobs.session))
		assert.Equal(t, 1, bobs.listChangedCount(), "bob's session got list_changed for alice's scope")

		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		require.Eventually(t, func() bool { return alices.listChangedCount() == 3 }, scopeChangeBound, 10*time.Millisecond)
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, alices.session))

		allowed := callTool(t, alices.session, "github__echo", map[string]any{"text": "back"})
		require.False(t, allowed.IsError, resultText(allowed))
		assert.Equal(t, "back", resultText(allowed))
	})

	t.Run("row 124: an enable waiting for its instance is refused at ready when the scope narrowed meanwhile", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		template := fixture.addEnableTemplate("github", "per-user", everyone)
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		client := fixture.connectObserved(t, alice())
		waiting := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")

		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultNone)
		fixture.markReady(t, instance.ID, "echo")

		outcome := awaitEnable(t, waiting)
		assert.Equal(t, unverifiedRefusal("github"), outcome.err)
	})

	t.Run("row 125: a user who lost catalog access stays refused although the scope allows everything", func(t *testing.T) {
		fixture := newMCPFixture(t, withWorkspaceScoping)
		fixture.addEnableTemplate("github", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		client := fixture.connectObserved(t, alice())

		assert.Empty(t, listServers(t, client.session))

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		assert.NotContains(t, resultText(result), "workspace")
	})
}

func TestWorkspaceScopeApprovalWait(t *testing.T) {
	t.Run("row 159: a call waiting for approval is refused with the step-4 message once the scope narrows, and the approval isn't used", func(t *testing.T) {
		defer gateway.SetApprovalWaitInterval(100 * time.Millisecond)()

		fixture := newMCPFixture(t, withApprovalURL, withWorkspaceScoping)
		template := fixture.addEnableTemplate("github", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})
		template.Spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Approval: policy.ApprovalRequired}}
		fixture.catalog.Set(template, nil)
		fixture.startUpstream(t, "github")
		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultAll)

		observed := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, observed, "github", "user:alice", "echo")

		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})
		outcomes := startEcho(t.Context(), client.session, echoParams())
		approval := fixture.awaitPendingApproval(t)
		fixture.awaitWaiting(t, client)

		fixture.setUnverifiedDefault(t, "alice", policy.ScopeDefaultNone)
		fixture.approve(t, approval.ID)

		outcome := awaitOutcome(t, outcomes, waitTimeout)
		require.NoError(t, outcome.err)
		require.True(t, outcome.result.IsError)
		assert.Equal(t, unverifiedRefusal("github"), resultText(outcome.result))
		assert.False(t, fixture.approvals(t)[0].UsedAt.Valid, "the approval isn't used")

		decisions := fixture.auditRows(t, "tool.decision")
		require.NotEmpty(t, decisions)
		last := decisions[len(decisions)-1]
		assert.Equal(t, "refuse", last.Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, last.Details["step"])
	})
}
