//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// ruleChangeBound is how soon after a rule change open sessions must get
// list_changed.
const ruleChangeBound = 5 * time.Second

func blockToolFor(principal, tool, reason string) *v1alpha1.ToolgateRule {
	return &v1alpha1.ToolgateRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: "block-" + principal},
		Spec: v1alpha1.ToolgateRuleSpec{
			Match:  v1alpha1.RuleMatch{Principals: []string{principal}, Tools: []string{tool}},
			Reason: reason,
		},
	}
}

// startCall calls tool in the background and returns its result, with a
// transport error turned into an error result.
func startCall(t *testing.T, session *mcp.ClientSession, tool string) <-chan *mcp.CallToolResult {
	t.Helper()

	calls := make(chan *mcp.CallToolResult, 1)

	go func() {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool})
		if err != nil {
			result = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
		}

		calls <- result
	}()

	return calls
}

func awaitCall(t *testing.T, calls <-chan *mcp.CallToolResult) *mcp.CallToolResult {
	t.Helper()

	select {
	case result := <-calls:
		return result
	case <-time.After(waitTimeout):
		require.FailNow(t, "the call did not finish")

		return nil
	}
}

func awaitStarted(t *testing.T, upstream *upstreamServer) {
	t.Helper()

	select {
	case <-upstream.started:
	case <-time.After(waitTimeout):
		require.FailNow(t, "the call never reached the upstream server")
	}
}

func TestRuleChanges(t *testing.T) {
	t.Run("a rule blocking a tool for one user withdraws it from that user's open session within 5 s and refuses its calls with the rule's reason; other users keep it", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		alices := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, alices, "github", "user:alice", "echo", "sized")
		require.Eventually(t, func() bool { return alices.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		bobs := fixture.connectObserved(t, bob())
		fixture.enableWithTools(t, bobs, "github", "user:bob", "echo", "sized")
		require.Eventually(t, func() bool { return bobs.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		require.NoError(t, fixture.rules.Set(blockToolFor("alice", "github__echo", "Echo is off for alice.")))

		require.Eventually(t, func() bool { return alices.listChangedCount() == 2 }, ruleChangeBound, 10*time.Millisecond,
			"alice's session got no list_changed within 5 s")
		assert.ElementsMatch(t, append(metaToolNames, "github__sized"), toolNames(t, alices.session))

		refused := callTool(t, alices.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, refused.IsError)
		assert.Contains(t, resultText(refused), "Echo is off for alice.")

		assert.ElementsMatch(t, append(metaToolNames, "github__echo", "github__sized"), toolNames(t, bobs.session))
		assert.Equal(t, 1, bobs.listChangedCount(), "bob's session got list_changed for alice's rule")

		allowed := callTool(t, bobs.session, "github__echo", map[string]any{"text": "still here"})
		require.False(t, allowed.IsError, resultText(allowed))
		assert.Equal(t, "still here", resultText(allowed))
	})

	t.Run("removing a rule brings the tools back to open sessions without re-enabling", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		rule := blockServer("github")
		require.NoError(t, fixture.rules.Set(rule))
		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, ruleChangeBound, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		fixture.rules.Delete(types.NamespacedName{Namespace: rule.Namespace, Name: rule.Name})
		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, ruleChangeBound, 10*time.Millisecond)
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "back"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "back", resultText(result))
	})

	t.Run("a rule reaching its expires time sends list_changed and brings the tools back", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		expires := time.Now().Add(2 * time.Second)
		rule := blockServer("github")
		rule.Spec.Expires = &metav1.Time{Time: expires}
		require.NoError(t, fixture.rules.Set(rule))
		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, ruleChangeBound, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, time.Until(expires)+ruleChangeBound, 10*time.Millisecond,
			"no list_changed when the rule expired")
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))
	})

	t.Run("a rule added while a call is in flight lets the call finish, refuses the next call and sends list_changed", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "slow")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		calls := startCall(t, client.session, "github__slow")
		awaitStarted(t, upstream)

		require.NoError(t, fixture.rules.Set(blockServer("github")))
		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, ruleChangeBound, 10*time.Millisecond)

		close(upstream.release)

		result := awaitCall(t, calls)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "slow done", resultText(result))

		later := callTool(t, client.session, "github__slow", nil)
		assert.True(t, later.IsError)
		assert.Contains(t, resultText(later), "GitHub is frozen")
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))
		assert.Len(t, upstream.started, 0, "the later call reached the upstream server")
	})
}

func TestAccessAndFailureChanges(t *testing.T) {
	t.Run("access removed from a user refuses calls at once and withdraws the tools from open sessions", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		// The template's access changes in the informer cache.
		changed := fixture.catalogTemplate(t, "github").DeepCopy()
		changed.Generation++
		changed.Spec.Access = v1alpha1.Access{Users: []string{"bob"}, Environments: "any"}
		fixture.catalog.Set(changed, nil)

		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, refused.IsError)
		assert.Contains(t, resultText(refused), "You don't have access to server \"github\".")

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, ruleChangeBound, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
	})

	t.Run("an instance failing after ready lets the in-flight call finish, refuses later calls with not-ready and keeps the server enabled", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, client, "github", "user:alice", "slow")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		calls := startCall(t, client.session, "github__slow")
		awaitStarted(t, upstream)

		// What the reconciler does when health checks fail: fail the row
		// and notify.
		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			if _, err := db.New(tx).SetInstanceFailed(t.Context(), db.SetInstanceFailedParams{ID: instance.ID, Reason: "health checks failed", FromStatus: "ready"}); err != nil {
				return err
			}

			return store.Notify(t.Context(), tx, store.KindInstance, instance.ID)
		}))

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		close(upstream.release)

		result := awaitCall(t, calls)
		assert.Equal(t, "slow done", resultText(result), "the in-flight call didn't get the upstream's answer")

		later := callTool(t, client.session, "github__slow", nil)
		assert.True(t, later.IsError)
		assert.Contains(t, resultText(later), "health checks failed")
		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 2)
		assert.Equal(t, "not-ready", decisions[1].Details["reason"])
	})
}
