//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

const (
	perUserLimitMessage = "You already have 2 servers running, the limit for each user. Disable servers you no longer need, or wait for idle ones to stop."
	gatewayLimitMessage = "The gateway is already running its limit of 2 servers. Try again later, or ask an admin to raise `limits.instances`."
)

func withInstanceLimits(perUser, total int) func(*gateway.MCPOptions) {
	return func(options *gateway.MCPOptions) {
		options.InstanceLimitPerUser = perUser
		options.InstanceLimit = total
	}
}

// enableReady enables name in session and makes its new instance ready, so
// the enable returns and the instance stays live.
func (f *mcpFixture) enableReady(t *testing.T, client *observedClient, name, scopeKey string) {
	t.Helper()

	results := startEnable(t, client.session, name)
	instance := f.awaitInstance(t, f.catalogTemplate(t, name), scopeKey)
	f.markReady(t, instance.ID, "search")
	require.Empty(t, awaitEnable(t, results).err)
}

// requireLimitRefusal checks that the enable of name was refused for the
// limit: nothing enabled, no instance, and a server.enable refusal with
// reason limit.
func (f *mcpFixture) requireLimitRefusal(t *testing.T, client user, name, limit string) {
	t.Helper()

	assert.NotContains(t, f.enabledServers(t, client.claims.Subject, client.session), name, "a refused enable leaves nothing enabled")
	assert.Empty(t, f.instancesOf(t, f.catalogTemplate(t, name)))

	events := f.auditEvents(t, "server.enable", name)
	require.Len(t, events, 1)
	assert.Equal(t, "refuse", events[0]["decision"])
	assert.Equal(t, "limit", events[0]["reason"])
	assert.Equal(t, limit, events[0]["limit"])
}

func TestInstanceLimits(t *testing.T) {
	t.Run("[AC-1-31] at the per-user limit a new instance is refused, reuse works, and other users are not affected", func(t *testing.T) {
		fixture := newMCPFixture(t, withInstanceLimits(2, 200))
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.addEnableTemplate("jira", "per-session", anyone)
		fixture.addEnableTemplate("slack", "per-user", anyone)

		first := fixture.connectObserved(t, alice())

		// The second instance is exactly at the limit, so it is allowed.
		fixture.enableReady(t, first, "github", "user:alice")
		fixture.enableReady(t, first, "jira", "session:alice:session-1")

		result := awaitEnable(t, startEnable(t, first.session, "slack"))
		assert.Equal(t, perUserLimitMessage, result.err)
		fixture.requireLimitRefusal(t, alice(), "slack", "per-user")

		// A per-user server already running for alice is reused from her
		// other session.
		secondUser := alice()
		secondUser.session = "session-2"
		second := fixture.connectObserved(t, secondUser)
		assert.Equal(t, enableResult{tools: []string{"github__search"}}, awaitEnable(t, startEnable(t, second.session, "github")))

		// Another user can still start servers.
		other := fixture.connectObserved(t, bob())
		fixture.enableReady(t, other, "slack", "user:bob")
	})

	t.Run("at the gateway limit an enable that needs a new instance is refused", func(t *testing.T) {
		fixture := newMCPFixture(t, withInstanceLimits(20, 2))
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.addEnableTemplate("wiki", "shared", anyone)
		fixture.addEnableTemplate("slack", "per-user", anyone)

		aliceClient := fixture.connectObserved(t, alice())
		bobClient := fixture.connectObserved(t, bob())

		// The second instance is exactly at the limit, so it is allowed.
		fixture.enableReady(t, aliceClient, "github", "user:alice")
		fixture.enableReady(t, bobClient, "wiki", "shared")

		result := awaitEnable(t, startEnable(t, bobClient.session, "slack"))
		assert.Equal(t, gatewayLimitMessage, result.err)
		fixture.requireLimitRefusal(t, bob(), "slack", "gateway")

		// Reusing a live instance works at the gateway limit.
		assert.Equal(t, enableResult{tools: []string{"wiki__search"}}, awaitEnable(t, startEnable(t, aliceClient.session, "wiki")))
	})

	t.Run("shared instances count only against the gateway limit", func(t *testing.T) {
		fixture := newMCPFixture(t, withInstanceLimits(1, 200))
		fixture.addEnableTemplate("wiki", "shared", anyone)
		fixture.addEnableTemplate("github", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		fixture.enableReady(t, client, "wiki", "shared")
		fixture.enableReady(t, client, "github", "user:alice")
	})
}

func TestInstanceLimitInteractions(t *testing.T) {
	t.Run("row 34: two enables of different servers by one user at the per-user limit take the last slot once", func(t *testing.T) {
		fixture := newMCPFixture(t, withInstanceLimits(1, 200))
		github := fixture.addEnableTemplate("github", "per-user", anyone)
		slack := fixture.addEnableTemplate("slack", "per-user", anyone)

		// Two sessions, so the sessions row locks don't serialize the enables.
		first := fixture.connectObserved(t, alice())
		secondUser := alice()
		secondUser.session = "session-2"
		second := fixture.connectObserved(t, secondUser)

		githubResults := startEnable(t, first.session, "github")
		slackResults := startEnable(t, second.session, "slack")

		// Each enable writes one server.enable event, allowed or refused.
		require.Eventually(t, func() bool {
			return len(fixture.auditEvents(t, "server.enable", "github"))+len(fixture.auditEvents(t, "server.enable", "slack")) == 2
		}, waitTimeout, 10*time.Millisecond)

		instances := append(fixture.instancesOf(t, github), fixture.instancesOf(t, slack)...)
		require.Len(t, instances, 1, "only one enable may take the last slot")
		fixture.markReady(t, instances[0].ID, "search")

		results := map[string]enableResult{"github": awaitEnable(t, githubResults), "slack": awaitEnable(t, slackResults)}
		winner := instances[0].TemplateName
		loser := map[string]string{"github": "slack", "slack": "github"}[winner]

		assert.Equal(t, enableResult{tools: []string{winner + "__search"}}, results[winner])
		assert.Equal(t, enableResult{err: "You already have 1 servers running, the limit for each user. Disable servers you no longer need, or wait for idle ones to stop."}, results[loser])
		enabledBy := map[string]user{"github": alice(), "slack": secondUser}
		fixture.requireLimitRefusal(t, enabledBy[loser], loser, "per-user")
	})
}
