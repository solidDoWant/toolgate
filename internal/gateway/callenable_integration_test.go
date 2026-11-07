//go:build integration

package gateway_test

import (
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestCallEnables(t *testing.T) {
	t.Run("row 175: two calls at once to a server that isn't enabled write one server.enable", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connect(t, alice())

		var wait sync.WaitGroup

		results := make([]*mcp.CallToolResult, 2)
		errs := make([]error, 2)

		for index := range results {
			wait.Add(1)

			go func() {
				defer wait.Done()

				results[index], errs[index] = client.CallTool(t.Context(), &mcp.CallToolParams{
					Name: "github__echo", Arguments: map[string]any{"text": "x"},
				})
			}()
		}

		instance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "github"), "user:alice")
		fixture.markReady(t, instance.ID, "echo")
		wait.Wait()

		for index := range results {
			require.NoError(t, errs[index])
			assert.False(t, results[index].IsError, resultText(results[index]))
		}

		enables := fixture.auditRows(t, "server.enable")
		require.Len(t, enables, 1)
		assert.Equal(t, "first-call", enables[0].Details["reason"])
		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"))
	})

	t.Run("a missing secret refuses the call with missing-secret and enables nothing", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
			`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"token"},"stringData":{"token":"${secret:github-token}"}}`,
		)}}
		fixture.catalog.Set(template, nil)

		client := fixture.connect(t, alice())
		result := callTool(t, client, "github__echo", nil)

		require.True(t, result.IsError)
		assert.Equal(t, "Secret `github-token` isn't set. Run `toolgate secret set github-token`.", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, "missing-secret", decisions[0].Details["reason"])
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))
		assert.Empty(t, fixture.auditRows(t, "server.enable"))
	})

	t.Run("row 176: a 33rd server refuses the call with limit", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connect(t, alice())
		row := fixture.sessionRow(t, "alice", "session-1")

		for index := range gateway.MaxEnabledServers {
			require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
				SessionPk: row.ID, TemplateNs: templateNamespace, TemplateName: "other-" + string(rune('a'+index/26)) + string(rune('a'+index%26)),
			}))
		}

		result := callTool(t, client, "github__echo", nil)
		require.True(t, result.IsError)
		assert.Equal(t, "This session already has 32 servers enabled. Disable one with `toolgate_disable_server` first.", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "limit", decisions[0].Details["reason"])
		assert.Empty(t, fixture.auditRows(t, "server.enable"))
	})

	t.Run("row 177: in a verified workspace a call outside the list asks, and an approval lets the next call through", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		client := fixture.connectObserved(t, attestedAlice())

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		require.True(t, result.IsError)

		requests := fixture.serverRequests(t)
		require.Len(t, requests, 1)
		assert.Equal(t, pendingAdditionMessage("github", requests[0].ID), resultText(result))
		assert.Equal(t, "pending", requests[0].Status)

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, decisions[0].Details["step"])
		assert.Equal(t, requests[0].ID, decisions[0].Details["request"])
		assert.Empty(t, fixture.auditRows(t, "server.enable"), "a call writes tool.decision, not server.enable, for its refusal")
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"))

		require.NoError(t, approveServerRequest(t, fixture, requests[0].ID, gateway.AdditionSession))

		calls := make(chan *mcp.CallToolResult, 1)

		go func() {
			result, _ := client.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "github__echo", Arguments: map[string]any{"text": "x"}})
			calls <- result
		}()

		instance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "github"), "user:alice")
		fixture.markReady(t, instance.ID, "echo")

		allowed := <-calls
		require.NotNil(t, allowed)
		require.False(t, allowed.IsError, resultText(allowed))

		enables := fixture.auditRows(t, "server.enable")
		require.Len(t, enables, 1)
		assert.Equal(t, "first-call", enables[0].Details["reason"])
	})

	t.Run("row 178: in an unverified session a call outside the list is refused at once", func(t *testing.T) {
		fixture := newMCPFixtureWithAuth(t, attestedUsers(), withWorkspaceScoping, withApprovalURL)
		fixture.addEnableTemplate("github", "per-user", aliceAndBob)
		fixture.startUpstream(t, "github")

		unverified := alice()
		unverified.session = "session-unverified"

		client := fixture.connect(t, unverified)
		result := callTool(t, client, "github__echo", nil)
		require.True(t, result.IsError)

		assert.Empty(t, fixture.serverRequests(t), "an unverified session never asks")

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, policy.StepWorkspaceScope, decisions[0].Details["step"])
	})

	t.Run("disable drops a server at once, and a verified workspace's enable outside the list asks", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		client := fixture.connectObserved(t, attestedAlice())

		result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
		require.True(t, result.IsError)
		require.Len(t, fixture.awaitServerRequests(t, 1), 1, "the enable outside the list asks")

		plain := newMCPFixture(t)
		plain.addEnableTemplate("github", "per-user", anyone)
		plain.startUpstream(t, "github")

		other := plain.connectObserved(t, alice())
		plain.enableWithTools(t, other, "github", "user:alice", "echo")
		require.Equal(t, []string{"github"}, plain.enabledServers(t, "alice", "session-1"))

		disable(t, other.session, "github")
		assert.Empty(t, plain.enabledServers(t, "alice", "session-1"))
	})
}
