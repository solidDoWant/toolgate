//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failed row of a deleted template (another UID) with the same name and the
// same generation number does not block calls of the re-created template.
func TestFailedRowOfRecreatedTemplateDoesNotBlockCalls(t *testing.T) {
	const scopeKey = "session:alice:session-1"

	fixture := newMCPFixture(t)
	template := fixture.lazyTemplate(t, "per-session", "echo")
	fixture.startUpstream(t, "github")

	old := template.DeepCopy()
	old.UID = "uid-github-deleted"

	client := fixture.connectObserved(t, alice())
	require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)
	fixture.failedInstance(t, old, scopeKey, template.Generation, "image pull failed")

	call := startWaitingCall(t, client, "github__echo", map[string]any{"text": "x"})

	require.Eventually(t, func() bool { return fixture.liveID(t, template, scopeKey) != "" }, 5*time.Second, 20*time.Millisecond,
		"the call should start an instance of the new template")

	fixture.markReady(t, fixture.liveID(t, template, scopeKey), "echo")
	assert.Equal(t, "x", resultText(awaitWaitingCall(t, call)))
}
