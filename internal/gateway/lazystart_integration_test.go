//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// lazyTemplate adds a per-session `github` template with a catalog list of
// the tools, which starts on the first call.
func (f *mcpFixture) lazyTemplate(t *testing.T, scope string, tools ...string) *v1alpha1.MCPServerTemplate {
	t.Helper()

	template := f.addEnableTemplate("github", scope, anyone)
	f.learn(t, string(template.UID), template.Generation, tools...)

	return template
}

// failedInstance inserts a failed instance row for the template.
func (f *mcpFixture) failedInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, scopeKey string, generation int64, reason string) {
	t.Helper()

	f.addInstance(t, template, scopeKey, generation, "failed", reason)
}

func TestLazyStart(t *testing.T) {
	const scopeKey = "session:alice:session-1"

	t.Run("enable of an onCall server with a catalog list inserts no instance, doesn't wait and lists the catalog tools", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		client := fixture.connectObserved(t, alice())

		result := awaitEnable(t, startEnable(t, client.session, "github"))

		require.Empty(t, result.err)
		assert.Equal(t, []string{"github__echo"}, result.tools)
		assert.Empty(t, fixture.instancesOf(t, template))
		assert.Contains(t, fixture.enabledServers(t, "alice", "session-1"), "github")

		events := fixture.auditEvents(t, "server.enable", "github")
		require.Len(t, events, 1)
		assert.Equal(t, "allow", events[0]["decision"])
		assert.NotContains(t, events[0], "instance")
	})

	t.Run("row 191: onEnable inserts the instance and waits", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		template.Spec.Start = catalog.StartOnEnable
		fixture.catalog.Set(template, nil)
		client := fixture.connectObserved(t, alice())

		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, scopeKey)
		fixture.markReady(t, instance.ID, "echo")

		require.Empty(t, awaitEnable(t, results).err)
	})

	t.Run("row 191: without a catalog list the enable inserts the instance and waits", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		client := fixture.connectObserved(t, alice())

		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, scopeKey)
		fixture.markReady(t, instance.ID, "echo")

		require.Empty(t, awaitEnable(t, results).err)
	})

	t.Run("row 234: an enable after a failure inserts a pending instance and doesn't wait", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.failedInstance(t, template, scopeKey, template.Generation, "image pull failed")
		client := fixture.connectObserved(t, alice())

		result := awaitEnable(t, startEnable(t, client.session, "github"))

		require.Empty(t, result.err)

		live, err := db.New(fixture.pool).GetLiveInstance(t.Context(), db.GetLiveInstanceParams{
			TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: scopeKey,
		})
		require.NoError(t, err)
		assert.Equal(t, "pending", live.Status)
	})

	t.Run("an enable after an idle stop inserts no instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.addInstance(t, template, scopeKey, template.Generation, "ready", "")
		fixture.stopInstance(t, fixture.liveID(t, template, scopeKey))
		client := fixture.connectObserved(t, alice())

		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		assert.Empty(t, fixture.liveID(t, template, scopeKey))
	})

	t.Run("row 192: a resume restores an onCall server without an instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		client := alice()
		client.workspace = "repo-a"

		first := fixture.connectObserved(t, client)
		require.Empty(t, awaitEnable(t, startEnable(t, first.session, "github")).err)

		fixture.endSession(t)

		resumed := fixture.connectObserved(t, client)

		assert.Contains(t, fixture.enabledServers(t, "alice", "session-1"), "github")
		assert.Empty(t, fixture.instancesOf(t, template))
		assert.Contains(t, toolNames(t, resumed.session), "github__echo")
	})

	t.Run("row 181: a call starts the instance, waits with progress and is forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		results := startWaitingCall(t, client, "github__echo", map[string]any{"text": "first"})
		instance := fixture.awaitInstance(t, template, scopeKey)
		fixture.assertWaiting(t, client, results)

		fixture.markReady(t, instance.ID, "echo")

		result := awaitWaitingCall(t, results)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "first", resultText(result))
		assert.Len(t, fixture.instancesOf(t, template), 1)
	})

	t.Run("a call refused by a rule after the enable inserts no instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		require.NoError(t, fixture.rules.Set(blockServer("github")))

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		require.True(t, result.IsError)

		assert.Empty(t, fixture.instancesOf(t, template))
		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
	})

	t.Run("row 182: two sessions of one user calling a per-user server get one instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-user", "echo")
		fixture.startUpstream(t, "github")

		first := fixture.connectObserved(t, alice())
		other := alice()
		other.session = "session-2"
		second := fixture.connectObserved(t, other)

		require.Empty(t, awaitEnable(t, startEnable(t, first.session, "github")).err)
		require.Empty(t, awaitEnable(t, startEnable(t, second.session, "github")).err)

		firstCall := startWaitingCall(t, first, "github__echo", map[string]any{"text": "one"})
		secondCall := startWaitingCall(t, second, "github__echo", map[string]any{"text": "two"})

		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "echo")

		assert.Equal(t, "one", resultText(awaitWaitingCall(t, firstCall)))
		assert.Equal(t, "two", resultText(awaitWaitingCall(t, secondCall)))
		assert.Len(t, fixture.instancesOf(t, template), 1)
	})

	t.Run("row 183: a start failure fails the waiting call with the reason", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		results := startWaitingCall(t, client, "github__echo", map[string]any{"text": "x"})
		instance := fixture.awaitInstance(t, template, scopeKey)
		fixture.assertWaiting(t, client, results)

		fixture.markFailed(t, instance.ID, "image pull failed")

		fixture.assertNotForwarded(t, awaitWaitingCall(t, results), "image pull failed")
	})

	t.Run("row 184: a call after a failure on the current generation is refused at once and inserts nothing", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)
		fixture.failedInstance(t, template, scopeKey, template.Generation, "image pull failed")

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})

		fixture.assertNotForwarded(t, result, "image pull failed")
		assert.Contains(t, resultText(result), "Enable `github` again to retry.")
		assert.Len(t, fixture.instancesOf(t, template), 1)
	})

	t.Run("row 235: a call after a failure on an older generation inserts an instance and waits", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)
		fixture.failedInstance(t, template, scopeKey, template.Generation-1, "image pull failed")

		results := startWaitingCall(t, client, "github__echo", map[string]any{"text": "again"})
		require.Eventually(t, func() bool { return fixture.liveID(t, template, scopeKey) != "" }, waitTimeout, 10*time.Millisecond)

		fixture.markReady(t, fixture.liveID(t, template, scopeKey), "echo")

		assert.Equal(t, "again", resultText(awaitWaitingCall(t, results)))
	})

	t.Run("row 186: a tool missing from the ready instance is refused as not offered", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo", "gone")
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		results := startWaitingCall(t, client, "github__gone", map[string]any{})
		instance := fixture.awaitInstance(t, template, scopeKey)
		fixture.markReady(t, instance.ID, "echo")

		result := awaitWaitingCall(t, results)

		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), "Server `github` doesn't offer `gone` any more.")

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "not-offered", decisions[0].Details["reason"])
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
	})

	t.Run("row 193: after an idle stop the next call starts a new instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		first := startWaitingCall(t, client, "github__echo", map[string]any{"text": "one"})
		old := fixture.awaitInstance(t, template, scopeKey)
		fixture.markReady(t, old.ID, "echo")
		require.Equal(t, "one", resultText(awaitWaitingCall(t, first)))

		fixture.stopInstance(t, old.ID)

		second := startWaitingCall(t, client, "github__echo", map[string]any{"text": "two"})
		require.Eventually(t, func() bool { return fixture.liveID(t, template, scopeKey) != "" }, waitTimeout, 10*time.Millisecond)

		fixture.markReady(t, fixture.liveID(t, template, scopeKey), "echo")

		assert.Equal(t, "two", resultText(awaitWaitingCall(t, second)))
		assert.NotEqual(t, old.ID, fixture.liveID(t, template, scopeKey))
	})

	t.Run("row 194: an unhealthy ready instance is refused and no other instance starts", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.lazyTemplate(t, "per-session", "echo")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		first := startWaitingCall(t, client, "github__echo", map[string]any{})
		instance := fixture.awaitInstance(t, template, scopeKey)
		fixture.markReady(t, instance.ID, "echo")
		_ = awaitWaitingCall(t, first)
		fixture.setHealth(t, instance.ID, false, "probe failed")

		result := callTool(t, client.session, "github__echo", map[string]any{})

		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), "unhealthy")
		assert.Len(t, fixture.instancesOf(t, template), 1)
	})

	t.Run("a call at the instance limit is refused with reason limit", func(t *testing.T) {
		fixture := newMCPFixture(t, withInstanceLimits(200, 2))
		template := fixture.lazyTemplate(t, "per-session", "echo")
		other := fixture.addEnableTemplate("jira", "per-session", anyone)
		third := fixture.addEnableTemplate("slack", "per-session", anyone)
		fixture.addInstance(t, other, scopeKey, other.Generation, "ready", "")
		fixture.addInstance(t, third, scopeKey, third.Generation, "ready", "")
		client := fixture.connectObserved(t, alice())
		require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

		result := callTool(t, client.session, "github__echo", map[string]any{})

		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), gatewayLimitMessage)
		assert.Empty(t, fixture.instancesOf(t, template))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "limit", decisions[0].Details["reason"])
	})
}

func (f *mcpFixture) liveID(t *testing.T, template *v1alpha1.MCPServerTemplate, scopeKey string) string {
	t.Helper()

	live, err := db.New(f.pool).GetLiveInstance(t.Context(), db.GetLiveInstanceParams{
		TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: scopeKey,
	})
	if err != nil {
		return ""
	}

	return live.ID
}
