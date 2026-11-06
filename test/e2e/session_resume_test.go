//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// sessionPassBound is how long the reconciler takes to end an idle session
// or forget an ended one's servers once it is due.
func sessionPassBound() time.Duration {
	return 2*scaled(runtime.DefaultPassInterval) + 30*time.Second
}

// idleOut moves the session's last_seen_at back past any idle timeout and
// waits for the reconciler to end it.
func idleOut(t *testing.T, admin *kubernetes.Clientset, session string) {
	t.Helper()

	gatewayQuery(t, admin, "UPDATE sessions SET last_seen_at = now() - interval '7 days' "+
		"WHERE principal = 'alice' AND session_id = '"+session+"'")

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		ended := gatewayQuery(t, admin, "SELECT ended_at IS NOT NULL FROM sessions "+
			"WHERE principal = 'alice' AND session_id = '"+session+"'")
		assert.Equal(collect, "t", ended)
	}, sessionPassBound(), time.Second)
}

// sessionEvents returns the session's audit events of the session.* types,
// as "type servers" lines in order.
func sessionEvents(t *testing.T, admin *kubernetes.Clientset, session string) []string {
	t.Helper()

	output := gatewayQuery(t, admin, "SELECT type || ' ' || coalesce(details->>'servers', '') FROM audit_events "+
		"WHERE principal = 'alice' AND session = '"+session+"' AND type LIKE 'session.%' ORDER BY id")

	lines := strings.Split(output, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}

	return lines
}

func TestAC3_22ResumeAfterLongPause(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	name := createTemplate(t, kube, "e2e-resume", "per-session", testServerResources(t, image, 0, nil))
	requireTemplateValid(t, kube, name)

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	// startAndPause enables the server in a new session, then lets the
	// session go idle past the timeout and checks nothing runs during the
	// pause.
	startAndPause := func(t *testing.T, session string) {
		t.Helper()

		client := connectGatewayInWorkspace(t, endpoint, "alice", session, "repo-a")
		assert.ElementsMatch(t, prefixedTools(name), enable(t, client, name))

		instance := requireOneInstance(t, admin, name)
		require.NoError(t, client.session.Close())

		idleOut(t, admin, session)
		requireNamespaceGone(t, admin, instance.Name, "nothing runs during the pause")
	}

	t.Run("[AC-3-22] a session resumed from the same workspace within the window gets its servers back", func(t *testing.T) {
		session := "e2e-" + rand.String(8)
		startAndPause(t, session)

		resumed := connectGatewayInWorkspace(t, endpoint, "alice", session, "repo-a")

		// A call made while the restored server is still starting waits for
		// it, with progress, instead of failing.
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()

		call := &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "early"}}
		call.SetProgressToken("resume-call")

		result, err := resumed.session.CallTool(ctx, call)
		require.NoError(t, err)
		require.False(t, result.IsError, "the call waits for the start instead of failing: %v", result.Content)
		require.Len(t, result.Content, 1)
		assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, `"early"`, "the call is forwarded")
		assert.Contains(t, resumed.recorded(), eventProgress, "the waiting call sends progress")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Contains(collect, resumed.recorded(), eventListChanged)
			assert.Subset(collect, listedTools(collect, t.Context(), resumed.session), prefixedTools(name))
		}, 2*time.Minute, time.Second, "the restored server's tools arrive with list_changed, without an enable")

		instance := requireOneInstance(t, admin, name)

		events := sessionEvents(t, admin, session)
		assert.Equal(t, []string{
			"session.start",
			`session.end ["` + name + `"]`,
			`session.resume ["` + name + `"]`,
		}, events, "the audit log records the set at the end and the resume")

		require.NoError(t, resumed.session.Close())
		idleOut(t, admin, session)
		requireNamespaceGone(t, admin, instance.Name, "the resumed session's instance stops when it ends again")
	})

	t.Run("[AC-3-22] a session resumed from another workspace starts with no servers", func(t *testing.T) {
		session := "e2e-" + rand.String(8)
		startAndPause(t, session)

		resumed := connectGatewayInWorkspace(t, endpoint, "alice", session, "repo-b")

		assert.NotContains(t, listedTools(t, t.Context(), resumed.session), prefixedTools(name)[0])
		assert.Empty(t, instanceNamespaces(t, t.Context(), admin, name), "nothing starts for the other workspace")
		assert.Equal(t, "session.start", sessionEvents(t, admin, session)[2])

		require.NoError(t, resumed.session.Close())
		idleOut(t, admin, session)
	})

	t.Run("[AC-3-22] a session resumed after the window starts with no servers, and the audit log records the forgetting", func(t *testing.T) {
		session := "e2e-" + rand.String(8)
		startAndPause(t, session)

		gatewayQuery(t, admin, "UPDATE sessions SET ended_at = now() - interval '8 days' "+
			"WHERE principal = 'alice' AND session_id = '"+session+"'")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Contains(collect, sessionEvents(t, admin, session), `session.forget ["`+name+`"]`)
		}, sessionPassBound(), time.Second)

		resumed := connectGatewayInWorkspace(t, endpoint, "alice", session, "repo-a")

		assert.NotContains(t, listedTools(t, t.Context(), resumed.session), prefixedTools(name)[0])
		assert.Empty(t, instanceNamespaces(t, t.Context(), admin, name), "nothing starts after the window")

		events := sessionEvents(t, admin, session)
		assert.Equal(t, "session.start", events[len(events)-1])

		require.NoError(t, resumed.session.Close())
		idleOut(t, admin, session)
	})
}
