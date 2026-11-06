//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §4 step 2: every call decides policy with fresh state before step 5
// inserts an instance; "If steps 1 to 3 refuse, refuse." A call of an
// enabled onCall server that a rule now blocks must be refused at once and
// start nothing.
func TestBughuntBlockedEnabledServerCallStartsInstance(t *testing.T) {
	fixture := newMCPFixture(t)
	template := fixture.lazyTemplate(t, "per-session", "echo")
	fixture.startUpstream(t, "github")
	client := fixture.connectObserved(t, alice())
	require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)
	require.Empty(t, fixture.instancesOf(t, template))

	require.NoError(t, fixture.rules.Set(blockServer("github")))

	call := startWaitingCall(t, client, "github__echo", map[string]any{"text": "x"})
	outcome := awaitWaitingCallOrNil(call)

	if outcome == nil {
		t.Errorf("the blocked call is waiting for a start instead of being refused at once")
	} else {
		assert.True(t, outcome.IsError)
		assert.Contains(t, resultText(outcome), "GitHub is frozen.")
	}

	assert.Empty(t, fixture.instancesOf(t, template), "a refused call must not start an instance")
}
