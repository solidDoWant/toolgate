//go:build integration

package gateway_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// waitingCallResult is the outcome of one proxied tool call made in the
// background.
type waitingCallResult struct {
	result *mcp.CallToolResult
	err    error
}

// waitingCall is a proxied tool call running in the background.
type waitingCall struct {
	results <-chan waitingCallResult
	// progressBefore is how many progress notifications the client had
	// before the call started.
	progressBefore int
}

// startWaitingCall calls a proxied tool in the background, with a progress
// token.
func startWaitingCall(t *testing.T, client *observedClient, name string, arguments map[string]any) waitingCall {
	t.Helper()

	results := make(chan waitingCallResult, 1)
	ctx := t.Context()
	call := waitingCall{results: results, progressBefore: len(client.progressMessages())}

	go func() {
		params := &mcp.CallToolParams{Name: name, Arguments: arguments}
		params.SetProgressToken("call-" + name)

		result, err := client.session.CallTool(ctx, params)
		results <- waitingCallResult{result: result, err: err}
	}()

	return call
}

// awaitWaitingCall waits for a call started with startWaitingCall. It
// allows less than the 10 s progress tick, so a wait that wakes only on the
// tick fails.
func awaitWaitingCall(t *testing.T, call waitingCall) *mcp.CallToolResult {
	t.Helper()

	select {
	case outcome := <-call.results:
		require.NoError(t, outcome.err)

		return outcome.result
	case <-time.After(enableTimeout):
		require.FailNow(t, "the call did not return")

		return nil
	}
}

// assertWaiting checks that a call is waiting: it has sent progress, it
// hasn't returned, and nothing about it was decided yet.
func (f *mcpFixture) assertWaiting(t *testing.T, client *observedClient, call waitingCall) {
	t.Helper()

	require.Eventually(t, func() bool { return len(client.progressMessages()) > call.progressBefore }, waitTimeout,
		10*time.Millisecond, "a waiting call sends progress")

	select {
	case outcome := <-call.results:
		require.FailNow(t, "the call returned instead of waiting", "%v", outcome)
	default:
	}

	assert.Empty(t, f.auditRows(t, "tool.decision"), "nothing is decided while the call waits")
}

// assertNotForwarded checks that a call was refused with message and never
// forwarded.
func (f *mcpFixture) assertNotForwarded(t *testing.T, result *mcp.CallToolResult, message string) {
	t.Helper()

	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), message)

	decisions := f.auditRows(t, "tool.decision")
	require.Len(t, decisions, 1)
	assert.Equal(t, "refuse", decisions[0].Details["decision"])
	assert.Equal(t, "not-ready", decisions[0].Details["reason"])
	assert.Empty(t, f.auditRows(t, "tool.result"), "nothing is forwarded")
}

// resumePending enables template's server in alice's session-1, ends the
// session and resumes it, and returns the resumed client and the restored
// instance, still pending.
func (f *mcpFixture) resumePending(t *testing.T, template *v1alpha1.MCPServerTemplate) (*observedClient, db.Instance) {
	t.Helper()

	client := alice()
	client.workspace = "repo-a"

	first := f.connectObserved(t, client)
	before := f.enableWithTools(t, first, template.Name, "session:alice:session-1", "echo")

	f.endSession(t)
	f.stopInstance(t, before.ID)

	resumed := f.connectObserved(t, client)

	instance := f.awaitInstance(t, template, "session:alice:session-1")
	require.NotEqual(t, before.ID, instance.ID)
	require.Equal(t, "pending", instance.Status)

	return resumed, instance
}

// replaceInstance does what the reconciler does when it replaces an
// instance for a restart or a repair: the old row is no longer desired and a
// pending replacement row is inserted, in one transaction.
func (f *mcpFixture) replaceInstance(t *testing.T, old db.Instance) string {
	t.Helper()

	instanceCounter++
	id := fmt.Sprintf("%032x", instanceCounter)

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.SetInstanceDesiredStopped(t.Context(), old.ID); err != nil {
			return err
		}

		if _, err := queries.InsertReplacementInstance(t.Context(), db.InsertReplacementInstanceParams{
			ID: id, TemplateNs: old.TemplateNs, TemplateName: old.TemplateName, TemplateUid: old.TemplateUid,
			Generation: old.Generation, Scope: old.Scope, ScopeKey: old.ScopeKey, OwnerPrincipal: old.OwnerPrincipal,
			RenderSession: old.RenderSession, Namespace: "instance-" + id[24:], TraceContext: old.TraceContext,
			LastUsedAt: old.LastUsedAt, Repairs: old.Repairs,
		}); err != nil {
			return err
		}

		if err := store.Notify(t.Context(), tx, store.KindInstance, old.ID); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, id)
	}))

	return id
}

func TestCallWaitsForStartingInstance(t *testing.T) {
	t.Run("a call to a restored server that is still pending waits, with progress, and is forwarded once it is ready", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		resumed, instance := fixture.resumePending(t, template)

		results := startWaitingCall(t, resumed, "github__echo", map[string]any{"text": "early"})
		fixture.assertWaiting(t, resumed, results)

		fixture.markReady(t, instance.ID, "echo")

		result := awaitWaitingCall(t, results)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "early", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "allow", decisions[0].Details["decision"])
		assert.Equal(t, instance.ID, decisions[0].Details["instance"])
		assert.Len(t, fixture.auditRows(t, "tool.result"), 1)
	})

	t.Run("a call waiting for an instance that fails is refused with the reason and not forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		resumed, instance := fixture.resumePending(t, template)

		results := startWaitingCall(t, resumed, "github__echo", map[string]any{"text": "early"})
		fixture.assertWaiting(t, resumed, results)

		fixture.markFailed(t, instance.ID, "image pull failed")

		fixture.assertNotForwarded(t, awaitWaitingCall(t, results), "image pull failed")
	})

	t.Run("a call waiting longer than startTimeout is refused and not forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		resumed, _ := fixture.resumePending(t, template)

		short := template.DeepCopy()
		short.Spec.StartTimeout = &metav1.Duration{Duration: 2 * time.Second}
		fixture.catalog.Set(short, nil)

		results := startWaitingCall(t, resumed, "github__echo", map[string]any{"text": "early"})

		fixture.assertNotForwarded(t, awaitWaitingCall(t, results), "`github` didn't become ready within 2s.")
	})

	t.Run("a call while a restart replaces a ready instance waits for the replacement and is forwarded to it", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		old := fixture.enableWithTools(t, client, "github", "session:alice:session-1", "echo")
		old, err := db.New(fixture.pool).GetInstance(t.Context(), old.ID)
		require.NoError(t, err)

		replacement := fixture.replaceInstance(t, old)

		results := startWaitingCall(t, client, "github__echo", map[string]any{"text": "during"})
		fixture.assertWaiting(t, client, results)

		fixture.markReady(t, replacement, "echo")

		result := awaitWaitingCall(t, results)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "during", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, replacement, decisions[0].Details["instance"])
	})

	t.Run("a disable while a call waits ends the wait with disabled while starting and forwards nothing", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		resumed, _ := fixture.resumePending(t, template)

		results := startWaitingCall(t, resumed, "github__echo", map[string]any{"text": "early"})
		fixture.assertWaiting(t, resumed, results)

		other := alice()
		other.workspace = "repo-a"
		disable(t, fixture.connectObserved(t, other).session, "github")

		fixture.assertNotForwarded(t, awaitWaitingCall(t, results), "`github` was disabled while starting.")
	})

	t.Run("a session that ends while a call waits ends the wait and keeps the server", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		resumed, _ := fixture.resumePending(t, template)

		results := startWaitingCall(t, resumed, "github__echo", map[string]any{"text": "early"})
		fixture.assertWaiting(t, resumed, results)

		ended := fixture.sessionRow(t, "alice", "session-1")
		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE id = $1", ended.ID); err != nil {
				return err
			}

			return store.Notify(t.Context(), tx, store.KindSession, fmt.Sprint(ended.ID))
		}))

		fixture.assertNotForwarded(t, awaitWaitingCall(t, results), "This session ended while waiting for `github`.")
		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"))
	})

	t.Run("a call to a failed instance with no live replacement is refused at once", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-session", anyone)
		fixture.startUpstream(t, "github")

		resumed, instance := fixture.resumePending(t, template)
		fixture.markFailed(t, instance.ID, "image pull failed")

		result := callTool(t, resumed.session, "github__echo", map[string]any{"text": "early"})
		fixture.assertNotForwarded(t, result, "image pull failed")
		assert.Empty(t, resumed.progressMessages(), "a refused call doesn't wait")
	})
}
