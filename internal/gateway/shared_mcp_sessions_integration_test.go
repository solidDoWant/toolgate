//go:build integration

package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// roundRobin sends each request to the next replica in turn, with no
// affinity, and records every 404.
type roundRobin struct {
	header   http.Header
	replicas []*url.URL
	next     atomic.Uint64

	mu       sync.Mutex
	notFound []string
}

func (r *roundRobin) RoundTrip(request *http.Request) (*http.Response, error) {
	target := r.replicas[(r.next.Add(1)-1)%uint64(len(r.replicas))]

	request = request.Clone(request.Context())
	request.URL.Scheme, request.URL.Host = target.Scheme, target.Host
	request.Host = target.Host

	for name, values := range r.header {
		request.Header[name] = values
	}

	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusNotFound {
		r.mu.Lock()
		r.notFound = append(r.notFound, request.Method+" "+target.Host)
		r.mu.Unlock()
	}

	return response, err
}

func (r *roundRobin) notFounds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.notFound...)
}

// connectRoundRobin connects client through a balancer over every replica.
func (f *mcpFixture) connectRoundRobin(t *testing.T, client user, replicas ...string) (*observedClient, *roundRobin) {
	t.Helper()

	balancer := &roundRobin{header: f.headers(t, client)}

	for _, replica := range replicas {
		parsed, err := url.Parse(replica)
		require.NoError(t, err)

		balancer.replicas = append(balancer.replicas, parsed)
	}

	observed := &observedClient{}
	transport := &mcp.StreamableClientTransport{Endpoint: replicas[0], HTTPClient: &http.Client{Transport: balancer}}

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	observed.session = session

	return observed, balancer
}

// pingStatusOn sends a ping on an MCP session to one replica and returns
// the HTTP status and body.
func (f *mcpFixture) pingStatusOn(t *testing.T, replica string, client user, mcpSessionID string) (int, string) {
	t.Helper()

	server := f.server
	f.server = &httptest.Server{URL: replica}

	defer func() { f.server = server }()

	response := f.post(t, client, mcpSessionID, pingMessage())
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	return response.StatusCode, string(body)
}

// endIdleMCPSessions makes every MCP session look unused for an hour and
// runs the leader's idle end of connector MCP sessions.
func (f *mcpFixture) endIdleMCPSessions(t *testing.T) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE mcp_sessions SET last_seen_at = now() - interval '1 hour'")
	require.NoError(t, err)

	ender := &runtime.MCPSessionEnder{Pool: f.pool, IdleTimeout: 30 * time.Minute}
	require.NoError(t, ender.Run(t.Context()))
}

func TestSharedMCPSessions(t *testing.T) {
	t.Run("a session served by two replicas round-robin never gets 404", func(t *testing.T) {
		fixture := newMCPFixture(t)
		other := fixture.replica(t)

		fixture.addTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client, balancer := fixture.connectRoundRobin(t, alice(), fixture.server.URL, other.URL)
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		for request := range 50 {
			switch request % 4 {
			case 0:
				_, err := client.session.ListTools(t.Context(), nil)
				require.NoError(t, err, "request %d", request)
			case 1:
				result := callTool(t, client.session, "github__echo", map[string]any{"text": "hi"})
				require.False(t, result.IsError, "request %d: %s", request, resultText(result))
			case 2:
				result := callTool(t, client.session, gateway.ToolDisableServer, map[string]any{"name": "github"})
				require.False(t, result.IsError, "request %d: %s", request, resultText(result))
			case 3:
				result := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": "github"})
				require.False(t, result.IsError, "request %d: %s", request, resultText(result))
			}
		}

		assert.Empty(t, balancer.notFounds())
		assert.Equal(t, 1, gateway.HeldMCPSessions(fixture.handlers[0]))
		assert.Equal(t, 1, gateway.HeldMCPSessions(fixture.handlers[1]))
	})

	t.Run("a replica adopts a session only for the identity it is bound to", func(t *testing.T) {
		fixture := newMCPFixture(t)
		other := fixture.replica(t)

		mcpSessionID := fixture.initializeRaw(t, alice())

		unknownStatus, unknownBody := fixture.pingStatusOn(t, other.URL, alice(), "0123456789abcdef0123456789abcdef")
		require.Equal(t, http.StatusNotFound, unknownStatus)

		otherWorkspace := alice()
		otherWorkspace.workspace = "elsewhere"

		otherSession := alice()
		otherSession.session = "session-2"

		for name, client := range map[string]user{
			"another principal": bob(), "another workspace": otherWorkspace, "another toolgate session": otherSession,
		} {
			status, body := fixture.pingStatusOn(t, other.URL, client, mcpSessionID)
			assert.Equal(t, unknownStatus, status, name)
			assert.Equal(t, unknownBody, body, name)
		}

		// A row bound to another attestation refuses the request's
		// identity, which is a user's.
		_, err := fixture.pool.Exec(t.Context(), "UPDATE mcp_sessions SET attestation = 'attested' WHERE id = $1", mcpSessionID)
		require.NoError(t, err)

		status, body := fixture.pingStatusOn(t, other.URL, alice(), mcpSessionID)
		assert.Equal(t, unknownStatus, status, "another attestation")
		assert.Equal(t, unknownBody, body, "another attestation")

		_, err = fixture.pool.Exec(t.Context(), "UPDATE mcp_sessions SET attestation = 'user' WHERE id = $1", mcpSessionID)
		require.NoError(t, err)

		assert.Zero(t, gateway.HeldMCPSessions(fixture.handlers[1]), "no copy is built for another identity")

		status, _ = fixture.pingStatusOn(t, other.URL, alice(), mcpSessionID)
		assert.Equal(t, http.StatusOK, status, "the bound identity is served on the other replica")
		assert.Equal(t, 1, gateway.HeldMCPSessions(fixture.handlers[1]))
	})

	t.Run("a DELETE on one replica ends the session another replica holds and closes its GET stream", func(t *testing.T) {
		fixture := newMCPFixture(t)
		other := fixture.replica(t)

		mcpSessionID := fixture.initializeRaw(t, connectorClient())
		streamClosed := fixture.watchStream(t, connectorClient(), mcpSessionID)

		server := fixture.server
		fixture.server = &httptest.Server{URL: other.URL}
		require.Equal(t, http.StatusNoContent, fixture.deleteMCPSession(t, connectorClient(), mcpSessionID))
		fixture.server = server

		select {
		case <-streamClosed:
		case <-time.After(10 * time.Second):
			t.Fatal("the GET stream on the first replica stayed open")
		}

		require.Eventually(t, func() bool { return gateway.HeldMCPSessions(fixture.handlers[0]) == 0 }, 10*time.Second, 10*time.Millisecond)

		status, _ := fixture.pingStatusOn(t, server.URL, connectorClient(), mcpSessionID)
		assert.Equal(t, http.StatusNotFound, status)
		status, _ = fixture.pingStatusOn(t, other.URL, connectorClient(), mcpSessionID)
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("a helper session idle past the connector idle limit is still served on either replica", func(t *testing.T) {
		const idleTimeout = 200 * time.Millisecond

		fixture := newMCPFixture(t, withMCPSessionIdleTimeout(idleTimeout))
		other := fixture.replica(t)

		mcpSessionID := fixture.initializeRaw(t, alice())
		require.Eventually(t, func() bool { return gateway.HeldMCPSessions(fixture.handlers[0]) == 0 },
			30*idleTimeout, idleTimeout/5, "the replica releases its idle copy")

		fixture.endIdleMCPSessions(t)

		status, _ := fixture.pingStatusOn(t, fixture.server.URL, alice(), mcpSessionID)
		assert.Equal(t, http.StatusOK, status, "the replica that held it adopts it again")
		status, _ = fixture.pingStatusOn(t, other.URL, alice(), mcpSessionID)
		assert.Equal(t, http.StatusOK, status, "the other replica adopts it")
	})
}

// watchStream opens the GET stream of an MCP session and returns a channel
// that closes when the gateway ends the stream.
func (f *mcpFixture) watchStream(t *testing.T, client user, mcpSessionID string) <-chan struct{} {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL, nil)
	require.NoError(t, err)

	request.Header = f.headers(t, client)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Mcp-Session-Id", mcpSessionID)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	closed := make(chan struct{})

	go func() {
		defer close(closed)

		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	return closed
}
