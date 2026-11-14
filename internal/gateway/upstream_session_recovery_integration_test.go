//go:build integration

package gateway_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
)

func TestForwardStartsNewUpstreamSessionAfterRestart(t *testing.T) {
	tests := map[string]struct {
		notFoundBody string
	}{
		"a 404 with a plain body": {},
		"a 404 with a JSON-RPC error body": {
			notFoundBody: `{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found"},"id":null}`,
		},
		// The go-sdk reads a JSON-RPC error with an ID as the server's
		// answer, not as a lost session.
		"a 404 with a JSON-RPC error body carrying an ID": {
			notFoundBody: `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Session not found"},"id":"server-error"}`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newMCPFixture(t)
			fixture.addEnableTemplate("github", "per-user", anyone)
			upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})
			upstream.mu.Lock()
			upstream.notFoundBody = test.notFoundBody
			upstream.mu.Unlock()

			client := fixture.connectObserved(t, alice())
			fixture.sessionfulInstance(t, client, "github", "user:alice")

			old := callSessionID(t, client.session, "github")

			// The server restarted and forgot every session it had.
			upstream.forget(old)

			renewed := callSessionID(t, client.session, "github")
			assert.NotEqual(t, old, renewed, "the call is retried on a new upstream session")
			assert.Equal(t, 2, upstream.initializeCount())

			rows := fixture.upstreamSessionRows(t)
			require.Len(t, rows, 1, "the row of the forgotten session is removed")
			assert.Equal(t, renewed, rows[0].upstream)
			assert.True(t, rows[0].live)

			assert.Equal(t, renewed, callSessionID(t, client.session, "github"), "later calls use the new upstream session")
		})
	}
}

func TestForwardRetriesOnlyOnceAfterNotFound(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "user:alice")
	callSessionID(t, client.session, "github")

	// Every upstream session is forgotten as soon as it is made.
	upstream.mu.Lock()
	upstream.forgetCalls = true
	upstream.mu.Unlock()

	result := callTool(t, client.session, "github__session_id", nil)
	require.True(t, result.IsError)
	assert.Contains(t, resultText(result), "Server `github` returned an error")
	assert.Equal(t, 2, upstream.initializeCount(), "the call is retried on one new upstream session, and only once")
}

func TestConcurrentNotFoundOnTwoReplicasKeepsOneRow(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})
	other := fixture.replica(t)

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "user:alice")
	second := fixture.connectObservedTo(t, other.URL, alice())

	old := callSessionID(t, first.session, "github")
	require.Equal(t, old, callSessionID(t, second.session, "github"))

	upstream.forget(old)

	calls := []<-chan *mcp.CallToolResult{startSessionIDCall(t.Context(), first.session, "github"), startSessionIDCall(t.Context(), second.session, "github")}

	var seen []string

	for _, call := range calls {
		result := awaitSessionIDCall(t, call)
		require.False(t, result.IsError, resultText(result))
		seen = append(seen, resultText(result))
	}

	assert.Equal(t, seen[0], seen[1], "both replicas end on one new upstream session")
	assert.NotEqual(t, "session:"+old, seen[0])

	rows := fixture.upstreamSessionRows(t)
	require.Len(t, rows, 1, "one row is left, and it is live")
	assert.True(t, rows[0].live)
	assert.Equal(t, seen[0], "session:"+rows[0].upstream)
}

func TestSessionlessFactIsForgottenWhenInstanceLeavesReady(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)

	var sessions atomic.Bool

	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{sessionID: func() string {
		if sessions.Load() {
			return uuid.NewString()
		}

		return ""
	}})

	client := fixture.connectObserved(t, alice())
	instanceID := fixture.sessionfulInstance(t, client, "github", "user:alice")

	assert.Empty(t, callSessionID(t, client.session, "github"))
	assert.Empty(t, callSessionID(t, client.session, "github"))
	require.Equal(t, 1, upstream.initializeCount(), "the replica remembers that the server is sessionless")

	// The sweep restarts the instance at a new generation, whose server
	// has sessions.
	sessions.Store(true)
	require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "UPDATE instances SET status = 'pending' WHERE id = $1", instanceID); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, instanceID)
	}))
	require.Eventually(t, func() bool { return gateway.HeldUpstreamCopies(fixture.handlers[0]) == 0 },
		waitTimeout, 10*time.Millisecond, "the instance's copy is released")

	fixture.markReady(t, instanceID, "session_id")
	fixture.setRevision(t, instanceID, sessionfulRevision)

	id := callSessionID(t, client.session, "github")
	assert.NotEmpty(t, id, "the first forward after the restart learns the server again")
	assert.Equal(t, 2, upstream.initializeCount())

	rows := liveRows(fixture.upstreamSessionRows(t))
	require.Len(t, rows, 1)
	assert.Equal(t, id, rows[0].upstream)
}

func TestDisableReleasesCopyOnEveryReplica(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})
	other := fixture.replica(t)

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "user:alice")
	second := fixture.connectObservedTo(t, other.URL, alice())

	require.Equal(t, callSessionID(t, first.session, "github"), callSessionID(t, second.session, "github"))
	require.Equal(t, 1, gateway.HeldUpstreamCopies(fixture.handlers[0]))
	require.Equal(t, 1, gateway.HeldUpstreamCopies(fixture.handlers[1]))

	disable(t, first.session, "github")

	for index, handler := range fixture.handlers {
		require.Eventually(t, func() bool { return gateway.HeldUpstreamCopies(handler) == 0 },
			waitTimeout, 10*time.Millisecond, "replica %d releases its copy", index)
	}
}

func TestIdleCopyIsReleasedWithoutDelete(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})
	handler := fixture.handlers[0]

	start := time.Now()

	var elapsed atomic.Int64

	gateway.SetUpstreamClock(handler, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "user:alice")
	id := callSessionID(t, client.session, "github")
	require.Equal(t, 1, gateway.HeldUpstreamCopies(handler))

	elapsed.Store(int64(30*time.Minute - time.Nanosecond))
	gateway.ReleaseIdleUpstreamCopies(handler)
	assert.Equal(t, 1, gateway.HeldUpstreamCopies(handler), "a copy idle for less than 30 minutes stays")

	elapsed.Store(int64(30 * time.Minute))
	gateway.ReleaseIdleUpstreamCopies(handler)
	assert.Equal(t, 0, gateway.HeldUpstreamCopies(handler), "a copy idle for 30 minutes is released")

	rows := liveRows(fixture.upstreamSessionRows(t))
	require.Len(t, rows, 1, "the row stays")
	assert.Equal(t, id, rows[0].upstream)

	assert.Equal(t, id, callSessionID(t, client.session, "github"), "the next call adopts the row")
	assert.Equal(t, 1, upstream.initializeCount())

	// The released copy closed in the background; the call above was
	// answered on the same upstream session, so the copy sent no DELETE.
	assert.Empty(t, upstream.deletes(), "releasing a copy never ends the upstream session")
}
