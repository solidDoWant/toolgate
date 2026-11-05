//go:build integration

package gateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBughuntOversizedJSONResultIsReadUnbounded(t *testing.T) {
	const size = 256 << 20

	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)

	var written atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&message)
		w.Header().Set("Mcp-Session-Id", "raw")
		w.Header().Set("Content-Type", "application/json")
		switch message.Method {
		case "initialize":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"raw","version":"0"}}}`, message.ID)
		case "tools/call":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"`, message.ID)
			chunk := bytes.Repeat([]byte("a"), 1<<20)
			for range size >> 20 {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
			}
			_, _ = io.WriteString(w, `"}]}}`)
		default:
			if len(message.ID) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, message.ID)
		}
	}))
	t.Cleanup(func() { httpServer.CloseClientConnections(); httpServer.Close() })
	fixture.upstreams.Store("github", httpServer.URL)

	client := fixture.connectObserved(t, alice())
	fixture.enableWithTools(t, client, "github", "user:alice", "huge")

	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var stats runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > peak.Load() {
				peak.Store(stats.HeapAlloc)
			}
		}
	}()

	result := callTool(t, client.session, "github__huge", map[string]any{})
	close(stop)

	t.Logf("upstream wrote %d MiB; peak heap %d MiB; result error=%v text=%q", written.Load()>>20, peak.Load()>>20, result.IsError, resultText(result)[:min(200, len(resultText(result)))])
	assert.Less(t, written.Load(), int64(32<<20), "the gateway read far more than the 16 MiB result limit from the upstream")
}
