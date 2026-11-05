//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Design row 31: when a rule reaches its expires time, open sessions get
// list_changed and the tools come back.
func TestBughuntRuleExpiryReachesOpenSessions(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	fixture.startUpstream(t, "github")

	client := fixture.connectObserved(t, alice())
	fixture.enableWithTools(t, client, "github", "user:alice", "echo")
	require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

	rule := blockServer("github")
	rule.Spec.Expires = &metav1.Time{Time: time.Now().Add(3 * time.Second)}
	require.NoError(t, fixture.rules.Set(rule))
	require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, ruleChangeBound, 10*time.Millisecond)
	assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

	// The rule expires 3 s from its creation; give it 3 s plus the 5 s bound.
	time.Sleep(8 * time.Second)
	t.Logf("list_changed count after expiry: %d; tools: %v", client.listChangedCount(), toolNames(t, client.session))
	assert.Equal(t, 3, client.listChangedCount(), "no list_changed when the rule expired")
	assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session), "the tools didn't come back")
}
