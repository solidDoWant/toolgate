package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// seenRequest is one HTTP request an upstream received.
type seenRequest struct {
	httpMethod string
	rpcMethod  string
	sessionID  string
	// authorization is the request's Authorization header.
	authorization string
}

// recordingUpstream is a go-sdk server with one tool behind a handler that
// records every request. When notFound is set, it answers `tools/call` with
// HTTP 404 and notFound's body instead.
type recordingUpstream struct {
	url string

	mu       sync.Mutex
	seen     []seenRequest
	notFound func(w http.ResponseWriter)
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	upstream := &recordingUpstream{}

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := seenRequest{httpMethod: r.Method, sessionID: r.Header.Get("Mcp-Session-Id"), authorization: r.Header.Get("Authorization")}

		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			r.Body = io.NopCloser(bytes.NewReader(raw))

			if message, err := jsonrpc.DecodeMessage(raw); err == nil {
				if request, ok := message.(*jsonrpc.Request); ok {
					seen.rpcMethod = request.Method
				}
			}
		}

		upstream.mu.Lock()
		upstream.seen = append(upstream.seen, seen)
		notFound := upstream.notFound
		upstream.mu.Unlock()

		if notFound != nil && seen.rpcMethod == "tools/call" {
			notFound(w)

			return
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	upstream.url = httpServer.URL

	return upstream
}

func (u *recordingUpstream) requests() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]seenRequest(nil), u.seen...)
}

func (u *recordingUpstream) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.seen = nil
}

func (u *recordingUpstream) answerNotFound(write func(w http.ResponseWriter)) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.notFound = write
}

// count returns how many requests had the HTTP method and JSON-RPC method.
func (u *recordingUpstream) count(httpMethod, rpcMethod string) int {
	found := 0

	for _, seen := range u.requests() {
		if seen.httpMethod == httpMethod && seen.rpcMethod == rpcMethod {
			found++
		}
	}

	return found
}

// forwardingCopy opens a forwarding connection to upstream that keeps the
// upstream session open at close unless deleteOnClose is set to true.
func forwardingCopy(t *testing.T, upstream *recordingUpstream, adopt *runtime.AdoptedSession) (*mcp.ClientSession, *atomic.Bool) {
	t.Helper()

	deleteOnClose := &atomic.Bool{}

	session, _, err := runtime.ConnectUpstream(t.Context(), upstream.url, runtime.RevisionStateful, runtime.UpstreamOptions{
		Adopt: adopt, DeleteOnClose: deleteOnClose,
	})
	require.NoError(t, err)

	return session, deleteOnClose
}

func TestAdoptedSessionSendsStoredIDWithoutInitialize(t *testing.T) {
	t.Parallel()

	upstream := newRecordingUpstream(t)

	first, _ := forwardingCopy(t, upstream, nil)
	t.Cleanup(func() { _ = first.Close() })

	require.NotEmpty(t, first.ID())

	result, err := json.Marshal(first.InitializeResult())
	require.NoError(t, err)

	upstream.reset()

	adopted, _ := forwardingCopy(t, upstream, &runtime.AdoptedSession{ID: first.ID(), InitializeResult: result})
	t.Cleanup(func() { _ = adopted.Close() })

	assert.Equal(t, first.ID(), adopted.ID())
	assert.Equal(t, first.InitializeResult().ProtocolVersion, adopted.InitializeResult().ProtocolVersion)

	_, err = adopted.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)

	assert.Zero(t, upstream.count(http.MethodPost, "initialize"))
	assert.Zero(t, upstream.count(http.MethodPost, "notifications/initialized"))

	calls := 0

	for _, seen := range upstream.requests() {
		if seen.rpcMethod == "tools/call" {
			calls++

			assert.Equal(t, first.ID(), seen.sessionID)
		}
	}

	assert.Equal(t, 1, calls)
}

func TestAdoptNeedsARevisionWithSessions(t *testing.T) {
	t.Parallel()

	upstream := newRecordingUpstream(t)
	adopt := &runtime.AdoptedSession{ID: "stored", InitializeResult: json.RawMessage(`{}`)}

	for _, revision := range []string{"", runtime.RevisionStateless} {
		_, _, err := runtime.ConnectUpstream(t.Context(), upstream.url, revision, runtime.UpstreamOptions{Adopt: adopt})
		require.Error(t, err, "revision %q", revision)
	}

	assert.Empty(t, upstream.requests())
}

func TestForwardingCopiesReleaseWithoutDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		deleteOnClose bool
		wantDeletes   int
	}{
		{name: "release", deleteOnClose: false, wantDeletes: 0},
		{name: "end", deleteOnClose: true, wantDeletes: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			upstream := newRecordingUpstream(t)

			session, deleteOnClose := forwardingCopy(t, upstream, nil)
			require.NotEmpty(t, session.ID())

			deleteOnClose.Store(test.deleteOnClose)
			require.NoError(t, session.Close())

			assert.Equal(t, test.wantDeletes, upstream.count(http.MethodDelete, ""))
		})
	}
}

func TestForwardingCopiesOpenNoGetStream(t *testing.T) {
	t.Parallel()

	upstream := newRecordingUpstream(t)

	session, _ := forwardingCopy(t, upstream, nil)
	t.Cleanup(func() { _ = session.Close() })

	// A call after the handshake makes sure the go-sdk had its chance to
	// open the stream: it starts the GET right after `initialize`.
	_, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)

	assert.Zero(t, upstream.count(http.MethodGet, ""))
}

func TestCallToolAfterNotFoundMatchesErrSessionMissing(t *testing.T) {
	t.Parallel()

	upstream := newRecordingUpstream(t)

	session, _ := forwardingCopy(t, upstream, nil)
	t.Cleanup(func() { _ = session.Close() })

	upstream.answerNotFound(func(w http.ResponseWriter) {
		http.Error(w, "session not found", http.StatusNotFound)
	})

	flag := &runtime.SessionNotFound{}

	_, err := session.CallTool(runtime.WithSessionNotFound(t.Context(), flag), &mcp.CallToolParams{Name: "echo"})
	require.ErrorIs(t, err, mcp.ErrSessionMissing)
	assert.True(t, flag.Found())
}

func TestNotFoundWithJSONRPCBodyIsDetected(t *testing.T) {
	t.Parallel()

	upstream := newRecordingUpstream(t)

	session, _ := forwardingCopy(t, upstream, nil)
	t.Cleanup(func() { _ = session.Close() })

	flag := &runtime.SessionNotFound{}

	_, err := session.CallTool(runtime.WithSessionNotFound(t.Context(), flag), &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)
	assert.False(t, flag.Found(), "a call that succeeded")

	upstream.answerNotFound(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found"},"id":null}`)
	})

	_, err = session.CallTool(runtime.WithSessionNotFound(t.Context(), flag), &mcp.CallToolParams{Name: "echo"})
	require.Error(t, err)
	assert.True(t, flag.Found())
}
