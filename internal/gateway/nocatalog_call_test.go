//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §3 "If none exists, the template has no catalog list. Such a server
// behaves as in phase 3: an enable or a call starts it and waits", and row
// 235 / §4 step 5: a failed row of an older generation doesn't block calls;
// the call inserts a pending row of the current generation and waits.
func TestCallStartsNoCatalogServerAfterOlderGenerationFailure(t *testing.T) {
	const scopeKey = "session:alice:session-1"

	fixture := newMCPFixture(t)
	template := fixture.addEnableTemplate("github", "per-session", anyone)
	fixture.startUpstream(t, "github")
	client := fixture.connectObserved(t, alice())

	results := startEnable(t, client.session, "github")
	instance := fixture.awaitInstance(t, template, scopeKey)
	fixture.markFailed(t, instance.ID, "image pull failed")
	require.NotEmpty(t, awaitEnable(t, results).err)
	require.Contains(t, fixture.enabledServers(t, "alice", "session-1"), "github")

	// The admin fixes the template: its generation changes.
	fixed := template.DeepCopy()
	fixed.Generation++
	fixture.catalog.Set(fixed, nil)

	call := startWaitingCall(t, client, "github__echo", map[string]any{"text": "again"})

	require.Eventually(t, func() bool { return fixture.liveID(t, fixed, scopeKey) != "" }, 5*time.Second, 20*time.Millisecond,
		"the call should insert a pending instance of the current generation")

	fixture.markReady(t, fixture.liveID(t, fixed, scopeKey), "echo")
	assert.Equal(t, "again", resultText(awaitWaitingCall(t, call)))
}
