//go:build integration

package gateway_test

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

func withMCPSessionIdleTimeout(timeout time.Duration) func(*gateway.MCPOptions) {
	return func(options *gateway.MCPOptions) { options.MCPSessionIdleTimeout = timeout }
}

func pingMessage() map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}
}

// initializeRaw starts an MCP session with a raw initialize and returns its
// Mcp-Session-Id. It never sends DELETE.
func (f *mcpFixture) initializeRaw(t *testing.T, client user) string {
	t.Helper()

	response := f.post(t, client, "", initializeMessage())
	require.Equal(t, http.StatusOK, response.StatusCode)

	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	id := response.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, id)

	return id
}

// pingStatus sends a ping on an MCP session and returns the HTTP status.
func (f *mcpFixture) pingStatus(t *testing.T, client user, mcpSessionID string) int {
	t.Helper()

	response := f.post(t, client, mcpSessionID, pingMessage())
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	return response.StatusCode
}

// openStream opens the GET stream of an MCP session and keeps it open until
// the returned function is called.
func (f *mcpFixture) openStream(t *testing.T, client user, mcpSessionID string) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL, nil)
	require.NoError(t, err)

	request.Header = f.headers(t, client)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Mcp-Session-Id", mcpSessionID)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = io.Copy(io.Discard, response.Body)
	}()

	return func() {
		cancel()

		_ = response.Body.Close()

		<-done
	}
}

func TestMCPSessionIdleRelease(t *testing.T) {
	const idleTimeout = 500 * time.Millisecond

	t.Run("an idle MCP session is released and then gets 404", func(t *testing.T) {
		fixture := newMCPFixture(t, withMCPSessionIdleTimeout(idleTimeout))

		mcpSessionID := fixture.initializeRaw(t, alice())
		require.Equal(t, http.StatusOK, fixture.pingStatus(t, alice(), mcpSessionID))

		assert.Eventually(t, func() bool {
			return fixture.pingStatus(t, alice(), mcpSessionID) == http.StatusNotFound
		}, 30*idleTimeout, 3*idleTimeout, "a probe is itself a request, so probes are farther apart than the idle timeout")
	})

	t.Run("an open GET stream keeps the MCP session", func(t *testing.T) {
		fixture := newMCPFixture(t, withMCPSessionIdleTimeout(idleTimeout))

		mcpSessionID := fixture.initializeRaw(t, alice())
		closeStream := fixture.openStream(t, alice(), mcpSessionID)

		// A lower bound on the time the session stays without a request:
		// the stream alone must keep it.
		time.Sleep(4 * idleTimeout)
		assert.Equal(t, http.StatusOK, fixture.pingStatus(t, alice(), mcpSessionID))

		closeStream()

		assert.Eventually(t, func() bool {
			return fixture.pingStatus(t, alice(), mcpSessionID) == http.StatusNotFound
		}, 30*idleTimeout, 3*idleTimeout, "a probe is itself a request, so probes are farther apart than the idle timeout")
	})

	t.Run("regular requests keep the MCP session", func(t *testing.T) {
		fixture := newMCPFixture(t, withMCPSessionIdleTimeout(idleTimeout))

		mcpSessionID := fixture.initializeRaw(t, alice())

		for deadline := time.Now().Add(4 * idleTimeout); time.Now().Before(deadline); {
			require.Equal(t, http.StatusOK, fixture.pingStatus(t, alice(), mcpSessionID))
			time.Sleep(idleTimeout / 10)
		}
	})

	t.Run("sessions abandoned without DELETE give back their memory and goroutines", func(t *testing.T) {
		fixture := newMCPFixture(t, withMCPSessionIdleTimeout(idleTimeout))

		// One session first, so lazily started goroutines and pools are
		// part of the baseline.
		warmUp := fixture.initializeRaw(t, alice())
		require.Eventually(t, func() bool {
			return fixture.pingStatus(t, alice(), warmUp) == http.StatusNotFound
		}, 30*idleTimeout, 3*idleTimeout, "a probe is itself a request, so probes are farther apart than the idle timeout")

		baseGoroutines, baseHeap := goroutinesAndHeap()

		const abandoned = 500

		ids := make([]string, 0, abandoned)
		for range abandoned {
			ids = append(ids, fixture.initializeRaw(t, alice()))
		}

		require.Eventually(t, func() bool {
			goroutines, _ := goroutinesAndHeap()

			return goroutines <= baseGoroutines+10
		}, 60*time.Second, idleTimeout, "goroutines stay above the baseline of %d", baseGoroutines)

		_, heap := goroutinesAndHeap()
		// Each abandoned session held about 80 KB, 40 MB in all.
		assert.Less(t, heap, baseHeap+8<<20, "heap %d KiB, baseline %d KiB", heap>>10, baseHeap>>10)

		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, alice(), ids[0]))
		assert.Equal(t, http.StatusNotFound, fixture.pingStatus(t, alice(), ids[abandoned-1]))
	})
}

func goroutinesAndHeap() (int, uint64) {
	runtime.GC()

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	return runtime.NumGoroutine(), stats.HeapAlloc
}
