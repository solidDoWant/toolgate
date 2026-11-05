//go:build integration

package gateway_test

import (
	"io"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Clients that initialize and then go away without DELETE (a killed Claude
// Code, a client that switched replica) leave their MCP sessions behind.
func TestBughuntAbandonedMCPSessionsAreNeverReleased(t *testing.T) {
	fixture := newMCPFixture(t)

	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}

	first := ""
	before := heap()
	goroutinesBefore := runtime.NumGoroutine()

	const sessions = 2000
	for range sessions {
		response := fixture.post(t, alice(), "", initializeMessage())
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		if first == "" {
			first = response.Header.Get("Mcp-Session-Id")
		}
	}

	after := heap()
	t.Logf("%d abandoned MCP sessions: heap %d KiB -> %d KiB (%d bytes each); goroutines %d -> %d",
		sessions, before>>10, after>>10, (int64(after)-int64(before))/sessions, goroutinesBefore, runtime.NumGoroutine())

	// The first abandoned session still answers.
	response := fixture.post(t, alice(), first, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"})
	_, _ = io.Copy(io.Discard, response.Body)
	t.Logf("first abandoned session answers ping with HTTP %d", response.StatusCode)
	assert.Less(t, int64(after)-int64(before), int64(sessions*1024), "abandoned sessions are kept in memory")
}
