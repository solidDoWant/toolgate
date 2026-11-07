//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// switchableTransport sends every request to the gateway replica that
// target names, as a load balancer without affinity would, and counts the
// 404 answers. Claude Code does not initialize again after a 404, so a
// session that sees one is lost.
type switchableTransport struct {
	header   http.Header
	target   atomic.Pointer[url.URL]
	notFound atomic.Int32
}

func (s *switchableTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range s.header {
		request.Header[name] = values
	}

	target := s.target.Load()
	request.URL.Scheme = target.Scheme
	request.URL.Host = target.Host

	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusNotFound {
		s.notFound.Add(1)
	}

	return response, err
}

func TestSessionSurvivesGatewayPodDeletion(t *testing.T) {
	kube := templateClient(t)
	admin := adminClient(t)
	server := revisionTemplate(t, kube, "both")
	requireTemplateValid(t, kube, server)

	pods := gatewayPods(t, admin)
	require.Len(t, pods, 2)

	endpoints := make([]*url.URL, len(pods))

	for index, pod := range pods {
		parsed, err := url.Parse(forwardToGateway(t, admin, pod))
		require.NoError(t, err)

		endpoints[index] = parsed
	}

	// The helper headers of the plugin: the toolgate session and the
	// claimed workspace.
	header := gatewayHeader(t, "alice", "e2e-"+rand.String(8), http.Header{auth.WorkspaceHeader: {"shared-sessions"}})
	transport := &switchableTransport{header: header}
	transport.target.Store(endpoints[0])

	var (
		mu     sync.Mutex
		events []string
	)

	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()

		events = append(events, event)
	}
	count := func(event string) int {
		mu.Lock()
		defer mu.Unlock()

		return countEvents(events, event)
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-claude-code", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { record(eventListChanged) },
	})

	session, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoints[0].String(),
		HTTPClient: &http.Client{Transport: transport},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	require.Equal(t, revisionLegacy, session.InitializeResult().ProtocolVersion)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Positive(collect, count(eventListChanged))
	}, time.Minute, 100*time.Millisecond, "the GET stream on the first replica gets list_changed when it attaches")

	enabled := enable(t, &gatewaySession{session: session}, server)
	require.Contains(t, enabled, server+"__echo")

	// The next request goes to the other replica, and the replica that
	// holds the GET stream dies.
	transport.target.Store(endpoints[1])

	mu.Lock()
	events = nil
	mu.Unlock()

	grace := int64(0)
	require.NoError(t, admin.CoreV1().Pods(releaseNamespace()).Delete(t.Context(), pods[0], metav1.DeleteOptions{GracePeriodSeconds: &grace}))

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Positive(collect, count(eventListChanged))
	}, 2*time.Minute, 200*time.Millisecond, "the reconnected stream on the other replica gets list_changed when it attaches")

	text, isError := toolCall(t, session, server+"__echo", map[string]any{"text": "after the pod died"})
	require.False(t, isError, text)
	assert.Contains(t, text, "after the pod died")

	mu.Lock()
	events = nil
	mu.Unlock()

	replaceUpstreamTools(t, admin, server, "echo,added")

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Positive(collect, count(eventListChanged))
		assert.Contains(collect, listedTools(collect, t.Context(), session), server+"__added")
	}, scaledHealthInterval()+2*time.Minute, time.Second, "the tool-list change reaches the reconnected stream")

	assert.Zero(t, transport.notFound.Load(), "the session got no 404")
}
