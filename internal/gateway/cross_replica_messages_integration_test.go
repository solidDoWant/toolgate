//go:build integration

package gateway_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

// crossReplicaBound is how soon a change made on one replica must reach a
// GET stream on another.
const crossReplicaBound = 5 * time.Second

// replicaRoute picks the replica a client request goes to.
type replicaRoute func(request *http.Request, message jsonrpc.Message) *url.URL

// splitTransport sends each client request to the replica route picks,
// with header added, and records the IDs of the responses the client sends.
type splitTransport struct {
	header http.Header
	route  replicaRoute

	mu          sync.Mutex
	answeredIDs []any
}

func (s *splitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())

	var message jsonrpc.Message

	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}

		request.Body = io.NopCloser(bytes.NewReader(body))
		message, _ = jsonrpc.DecodeMessage(body)
	}

	if response, ok := message.(*jsonrpc.Response); ok {
		s.mu.Lock()
		s.answeredIDs = append(s.answeredIDs, response.ID.Raw())
		s.mu.Unlock()
	}

	target := s.route(request, message)

	request.URL.Scheme, request.URL.Host = target.Scheme, target.Host
	request.Host = target.Host

	for name, values := range s.header {
		request.Header[name] = values
	}

	return http.DefaultTransport.RoundTrip(request)
}

func (s *splitTransport) answered() []any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]any(nil), s.answeredIDs...)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	require.NoError(t, err)

	return parsed
}

// isToolCall reports whether message is a tools/call request.
func isToolCall(message jsonrpc.Message) bool {
	request, ok := message.(*jsonrpc.Request)

	return ok && request.Method == "tools/call"
}

// isResponse reports whether message is a client's response to a server
// request.
func isResponse(message jsonrpc.Message) bool {
	_, ok := message.(*jsonrpc.Response)

	return ok
}

// connectElicitingThrough connects alice with a client that declares URL
// elicitation on 2025-11-25 and answers every elicitation with decline,
// after ready returns.
func (f *mcpFixture) connectElicitingThrough(t *testing.T, transport http.RoundTripper, ready func()) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "eliciting", Version: "0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			ready()

			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: f.server.URL, HTTPClient: &http.Client{Transport: transport},
	}, &mcp.ClientSessionOptions{ProtocolVersion: revisionElicitCreate})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	_, err = session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	return session
}

// boundedCall calls the approval-required echo tool with text and gives up
// after waitTimeout, so an answer that never arrives fails the test.
func boundedCall(ctx context.Context, session *mcp.ClientSession, text string) callOutcome {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "github__echo", Arguments: map[string]any{"text": text}})

	return callOutcome{result: result, err: err}
}

func TestCrossReplicaMessages(t *testing.T) {
	t.Run("an elicitation sent on one replica and answered on another reaches the waiting call", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		asking, answering := mustParseURL(t, fixture.server.URL), mustParseURL(t, fixture.replica(t).URL)

		transport := &splitTransport{header: fixture.headers(t, alice()), route: func(_ *http.Request, message jsonrpc.Message) *url.URL {
			if isResponse(message) {
				return answering
			}

			return asking
		}}

		session := fixture.connectElicitingThrough(t, transport, func() {})

		outcome := boundedCall(t.Context(), session, "hi")
		require.NoError(t, outcome.err, "the decline reached the call")

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 1)
		require.True(t, outcome.result.IsError)
		assert.Equal(t, pendingMessage(approvals[0].ID), resultText(outcome.result))

		require.Len(t, transport.answered(), 1)
		assert.IsType(t, "", transport.answered()[0], "the client answers a request ID that names the replica")
		assert.Equal(t, 1, gateway.HeldMCPSessions(fixture.handlers[1]), "the answer went to the other replica")
	})

	t.Run("two calls of one session on two replicas get distinct request IDs and their own answers", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		first, second := mustParseURL(t, fixture.server.URL), mustParseURL(t, fixture.replica(t).URL)

		// The calls alternate between the replicas, and every answer goes
		// to the second, so the first call's answer crosses replicas while
		// the second call's is local.
		var calls atomic.Uint64

		transport := &splitTransport{header: fixture.headers(t, alice()), route: func(_ *http.Request, message jsonrpc.Message) *url.URL {
			switch {
			case isResponse(message):
				return second
			case isToolCall(message) && (calls.Add(1)-1)%2 == 1:
				return second
			default:
				return first
			}
		}}

		// No answer goes out until both calls wait for one, so the two
		// replicas' requests are pending at the same time. The calls have
		// different arguments, so each has its own approval.
		var pending sync.WaitGroup

		pending.Add(2)

		bothAsked := make(chan struct{})

		go func() {
			pending.Wait()
			close(bothAsked)
		}()

		session := fixture.connectElicitingThrough(t, transport, func() {
			pending.Done()

			select {
			case <-bothAsked:
			case <-time.After(waitTimeout):
			}
		})

		outcomes := make([]callOutcome, 2)

		var running sync.WaitGroup

		for index := range outcomes {
			running.Go(func() { outcomes[index] = boundedCall(t.Context(), session, fmt.Sprint("call ", index)) })
		}

		running.Wait()

		for index, outcome := range outcomes {
			require.NoError(t, outcome.err, "call %d got its answer", index)
			require.True(t, outcome.result.IsError, "call %d", index)
			assert.Contains(t, resultText(outcome.result), "needs approval", "call %d", index)
		}

		answered := transport.answered()
		require.Len(t, answered, 2)
		assert.NotEqual(t, answered[0], answered[1], "the two replicas' requests have distinct IDs")
	})

	t.Run("a disable on one replica sends list_changed on a GET stream on another", func(t *testing.T) {
		fixture := newMCPFixture(t)
		streaming, serving := mustParseURL(t, fixture.server.URL), mustParseURL(t, fixture.replica(t).URL)

		fixture.addTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		transport := &splitTransport{header: fixture.headers(t, alice()), route: func(request *http.Request, _ jsonrpc.Message) *url.URL {
			if request.Method == http.MethodGet {
				return streaming
			}

			return serving
		}}

		client := connectObservedThrough(t, fixture.server.URL, transport)
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() > 0 }, waitTimeout, 10*time.Millisecond)

		before := client.listChangedCount()

		result := callTool(t, client.session, gateway.ToolDisableServer, map[string]any{"name": "github"})
		require.False(t, result.IsError, resultText(result))

		require.Eventually(t, func() bool { return client.listChangedCount() > before }, crossReplicaBound, 10*time.Millisecond,
			"the GET stream's replica sends list_changed")
	})

	t.Run("a GET stream that attaches gets one list_changed at once", func(t *testing.T) {
		fixture := newMCPFixture(t)
		other := fixture.replica(t)

		mcpSessionID := fixture.initializeRaw(t, alice())

		for _, replica := range []string{fixture.server.URL, other.URL} {
			events := fixture.openStreamOn(t, replica, alice(), mcpSessionID)

			select {
			case event := <-events:
				assert.Contains(t, event, `"method":"notifications/tools/list_changed"`, replica)
			case <-time.After(crossReplicaBound):
				t.Fatalf("the GET stream on %s got no list_changed", replica)
			}

			select {
			case event := <-events:
				t.Fatalf("the GET stream on %s got a second event: %s", replica, event)
			case <-time.After(200 * time.Millisecond):
			}
		}
	})
}

// openStreamOn opens the GET stream of an MCP session on one replica and
// returns the data lines of its events. The stream closes with the test.
func (f *mcpFixture) openStreamOn(t *testing.T, replica string, client user, mcpSessionID string) <-chan string {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, replica, nil)
	require.NoError(t, err)

	request.Header = f.headers(t, client)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Mcp-Session-Id", mcpSessionID)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	events := make(chan string, 16)

	go func() {
		defer func() { _ = response.Body.Close() }()

		buffer := make([]byte, 4096)

		for {
			read, err := response.Body.Read(buffer)
			for _, line := range bytes.Split(buffer[:read], []byte("\n")) {
				if data, found := bytes.CutPrefix(line, []byte("data: ")); found {
					events <- string(data)
				}
			}

			if err != nil {
				return
			}
		}
	}()

	return events
}
