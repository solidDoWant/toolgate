package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// blockingUpstream is a go-sdk server whose tool `wait` blocks until the
// test releases it or the call is cancelled. It records the
// MCP-Protocol-Version header of each `tools/call`.
type blockingUpstream struct {
	url string
	// entered gets one value for each call that reached the tool.
	entered chan struct{}
	// cancelled gets one value for each call cancelled while it waited.
	cancelled chan struct{}
	release   chan struct{}

	mu       sync.Mutex
	versions []string
}

func newBlockingUpstream(t *testing.T) *blockingUpstream {
	t.Helper()

	upstream := &blockingUpstream{entered: make(chan struct{}, 8), cancelled: make(chan struct{}, 8), release: make(chan struct{})}

	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "wait"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		upstream.entered <- struct{}{}

		select {
		case <-upstream.release:
		case <-ctx.Done():
			upstream.cancelled <- struct{}{}
		}

		return &mcp.CallToolResult{}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") != "" {
			upstream.mu.Lock()
			upstream.versions = append(upstream.versions, r.Header.Get("MCP-Protocol-Version"))
			upstream.mu.Unlock()
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() {
		select {
		case <-upstream.release:
		default:
			close(upstream.release)
		}
	})

	upstream.url = httpServer.URL

	return upstream
}

func (u *blockingUpstream) awaitEntered(t *testing.T) {
	t.Helper()

	select {
	case <-u.entered:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no call reached the tool")
	}
}

func (u *blockingUpstream) sentVersions() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]string(nil), u.versions...)
}

// forwardingCopyOf opens a forwarding connection to the upstream at url,
// adopting adopt when it is set.
func forwardingCopyOf(t *testing.T, url string, adopt *runtime.AdoptedSession) *mcp.ClientSession {
	t.Helper()

	deleteOnClose := new(atomic.Bool)

	session, _, err := runtime.ConnectUpstream(t.Context(), url, runtime.RevisionStateful, runtime.UpstreamOptions{
		Adopt: adopt, DeleteOnClose: deleteOnClose,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// adoptedCopyOf opens a forwarding connection that adopts first's upstream
// session.
func adoptedCopyOf(t *testing.T, url string, first *mcp.ClientSession) *mcp.ClientSession {
	t.Helper()

	result, err := json.Marshal(first.InitializeResult())
	require.NoError(t, err)

	return forwardingCopyOf(t, url, &runtime.AdoptedSession{ID: first.ID(), InitializeResult: result})
}

func startWait(ctx context.Context, session *mcp.ClientSession) <-chan error {
	done := make(chan error, 1)

	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait"})
		done <- err
	}()

	return done
}

func awaitCall(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the call didn't finish")

		return nil
	}
}

func TestCopiesOfOneUpstreamSessionCallAtOnce(t *testing.T) {
	t.Parallel()

	upstream := newBlockingUpstream(t)
	first := forwardingCopyOf(t, upstream.url, nil)
	adopted := adoptedCopyOf(t, upstream.url, first)

	// Both copies number their first call alike; the second one is sent
	// while the first is still in flight on the shared session.
	firstCall := startWait(t.Context(), first)
	upstream.awaitEntered(t)

	adoptedCall := startWait(t.Context(), adopted)

	select {
	case <-upstream.entered:
	case err := <-adoptedCall:
		require.FailNow(t, "the adopted copy's call ended before reaching the tool", "error: %v", err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the adopted copy's call didn't reach the tool")
	}

	close(upstream.release)

	require.NoError(t, awaitCall(t, firstCall))
	require.NoError(t, awaitCall(t, adoptedCall))
}

func TestCancellingACallOfAnAdoptedCopyCancelsItUpstream(t *testing.T) {
	t.Parallel()

	upstream := newBlockingUpstream(t)
	session := adoptedCopyOf(t, upstream.url, forwardingCopyOf(t, upstream.url, nil))

	ctx, cancel := context.WithCancel(t.Context())
	call := startWait(ctx, session)

	upstream.awaitEntered(t)

	cancel()
	require.ErrorIs(t, awaitCall(t, call), context.Canceled)

	select {
	case <-upstream.cancelled:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the server's handler wasn't cancelled")
	}
}

func TestAdoptedCopiesSendTheNegotiatedProtocolVersion(t *testing.T) {
	t.Parallel()

	upstream := newBlockingUpstream(t)
	close(upstream.release)

	first := forwardingCopyOf(t, upstream.url, nil)
	adopted := adoptedCopyOf(t, upstream.url, first)

	for _, session := range []*mcp.ClientSession{first, adopted} {
		require.NoError(t, awaitCall(t, startWait(t.Context(), session)))
	}

	versions := upstream.sentVersions()
	require.NotEmpty(t, versions)

	for _, version := range versions {
		assert.Equal(t, first.InitializeResult().ProtocolVersion, version)
	}
}
