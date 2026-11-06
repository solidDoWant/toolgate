//go:build integration

package gateway_test

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Row 184 / §3 "A failed server isn't retried by calls": a call that
// implicitly enables an onCall server whose newest instance failed on the
// current generation must be refused at once and insert nothing.
func TestBughuntImplicitEnableRetriesFailedStart(t *testing.T) {
	const scopeKey = "session:alice:session-1"

	fixture := newMCPFixture(t)
	template := fixture.lazyTemplate(t, "per-session", "echo")
	fixture.startUpstream(t, "github")
	fixture.failedInstance(t, template, scopeKey, template.Generation, "image pull failed")
	client := fixture.connectObserved(t, alice())

	// The server is not enabled: the call enables it (§4 step 3).
	call := startWaitingCall(t, client, "github__echo", map[string]any{"text": "x"})

	select {
	case outcome := <-call.results:
		require.NoError(t, outcome.err)
		assert.Contains(t, resultText(outcome.result), "Enable `github` again to retry.")
	case <-t.Context().Done():
	default:
		// Give the call a moment to finish (it should refuse at once).
		outcome := awaitWaitingCallOrNil(call)
		if outcome == nil {
			t.Errorf("the call is waiting instead of being refused at once; instances now: %d", len(fixture.instancesOf(t, template)))
		} else {
			assert.Contains(t, resultText(outcome), "Enable `github` again to retry.")
		}
	}

	assert.Len(t, fixture.instancesOf(t, template), 1, "a call must not insert a new instance after a failure on the current generation")
}

func awaitWaitingCallOrNil(call waitingCall) *mcp.CallToolResult {
	select {
	case outcome := <-call.results:
		if outcome.err != nil {
			return nil
		}

		return outcome.result
	case <-time.After(3 * time.Second):
		return nil
	}
}

// §3/§4 step 5: only a failed row of the template's current generation
// blocks calls. A failed row of a deleted template (another UID) that had
// the same name and the same generation number isn't the current template's.
func TestBughuntFailedRowOfRecreatedTemplateBlocksCalls(t *testing.T) {
	const scopeKey = "session:alice:session-1"

	fixture := newMCPFixture(t)
	template := fixture.lazyTemplate(t, "per-session", "echo")
	fixture.startUpstream(t, "github")

	// The deleted predecessor: same name, other UID, same generation number.
	old := template.DeepCopy()
	old.UID = "uid-github-deleted"
	// The session enabled `github` while the old template existed; its
	// instance failed; the admin deleted and re-created the template, which
	// starts again at the same generation number. The enabled row stays.
	client := fixture.connectObserved(t, alice())
	require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)
	fixture.failedInstance(t, old, scopeKey, template.Generation, "image pull failed")

	call := startWaitingCall(t, client, "github__echo", map[string]any{"text": "x"})

	started := assert.Eventually(t, func() bool { return fixture.liveID(t, template, scopeKey) != "" }, 5*time.Second, 20*time.Millisecond,
		"the call should start an instance of the new template")
	if !started {
		if outcome := awaitWaitingCallOrNil(call); outcome != nil {
			t.Logf("call result: %q", resultText(outcome))
		}

		return
	}

	fixture.markReady(t, fixture.liveID(t, template, scopeKey), "echo")
	assert.Equal(t, "x", resultText(awaitWaitingCall(t, call)))
}
