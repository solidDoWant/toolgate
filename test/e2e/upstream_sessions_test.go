//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

// connectThroughReplicas connects a toolgate session of user whose requests
// go to the replica that the returned transport targets.
func connectThroughReplicas(t *testing.T, user string, endpoints []*url.URL) (*mcp.ClientSession, *switchableTransport) {
	t.Helper()

	header := gatewayHeader(t, user, "e2e-"+rand.String(8), http.Header{auth.WorkspaceHeader: {"upstream-sessions"}})
	transport := &switchableTransport{header: header}
	transport.target.Store(endpoints[0])

	client := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-claude-code", Version: "0"}, nil)

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoints[0].String(),
		HTTPClient: &http.Client{Transport: transport},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session, transport
}

func TestStatefulServerKeepsOneUpstreamSessionAcrossReplicas(t *testing.T) {
	kube := templateClient(t)
	admin := adminClient(t)

	server := statefulSessionTemplate(t, kube)
	requireTemplateValid(t, kube, server)

	pods := gatewayPods(t, admin)
	require.Len(t, pods, 2)

	endpoints := make([]*url.URL, len(pods))

	for index, pod := range pods {
		parsed, err := url.Parse(forwardToGateway(t, admin, pod))
		require.NoError(t, err)

		endpoints[index] = parsed
	}

	first, firstTransport := connectThroughReplicas(t, "alice", endpoints)
	second, _ := connectThroughReplicas(t, "alice", endpoints)

	require.Contains(t, enable(t, &gatewaySession{session: first}, server), server+"__session_id")
	require.Contains(t, enable(t, &gatewaySession{session: second}, server), server+"__session_id")

	sessionID := func(session *mcp.ClientSession) string {
		text, isError := toolCall(t, session, server+"__session_id", nil)
		require.False(t, isError, text)
		require.NotEmpty(t, text)

		return text
	}

	firstTransport.target.Store(endpoints[0])

	idOnFirstReplica := sessionID(first)

	firstTransport.target.Store(endpoints[1])

	idOnSecondReplica := sessionID(first)

	assert.Equal(t, idOnFirstReplica, idOnSecondReplica, "both replicas use one upstream session")
	assert.NotEqual(t, idOnFirstReplica, sessionID(second), "another toolgate session gets its own upstream session")

	liveSessions := func() int {
		text, isError := toolCall(t, second, server+"__live_sessions", nil)
		require.False(t, isError, text)

		count, err := strconv.Atoi(strings.TrimSpace(text))
		require.NoError(t, err, text)

		return count
	}

	// The gateway holds sessions of its own, for the tool catalog, so only
	// the change is exact.
	before := liveSessions()
	require.GreaterOrEqual(t, before, 2, "the server holds a session for each toolgate session")

	result := callServerTool(t.Context(), first, gateway.ToolDisableServer, server)
	require.Empty(t, result.err)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Less(collect, liveSessions(), before)
	}, time.Minute, time.Second, "disable ends the upstream session of the first toolgate session")

	assert.Zero(t, firstTransport.notFound.Load(), "the session got no 404")
}

// statefulSessionTemplate creates a per-user template of the stateful test
// server with the session tools, and returns its name.
func statefulSessionTemplate(t *testing.T, kube client.Client) string {
	t.Helper()

	return createTemplate(t, kube, "e2e-upsess", "per-user",
		testServerResourcesWithTools(t, mcpServerImage(t), []string{"echo", "session_id", "live_sessions"}, 0,
			map[string]string{"MCPSERVER_MODE": "2025-only"}))
}
