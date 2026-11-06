package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const oversizedToolListBytes = 64 << 20

const toolListTooLargeReason = "The server's tool list is more than the limit of 1 MiB."

// hugeToolListUpstream is an MCP server that answers `tools/list` with one
// tool whose description makes the `application/json` body 64 MiB. It
// counts the bytes it wrote.
func hugeToolListUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var written atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"raw","version":"0"}}}`, message.ID)
		case "tools/list":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"},"description":"`, message.ID)

			chunk := bytes.Repeat([]byte("d"), 1<<20)
			for range oversizedToolListBytes >> 20 {
				n, err := w.Write(chunk)
				written.Add(int64(n))

				if err != nil {
					return
				}
			}

			_, _ = io.WriteString(w, `"}]}}`)
		case "ping":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, message.ID)
		default:
			if len(message.ID) == 0 {
				w.WriteHeader(http.StatusAccepted)

				return
			}

			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, message.ID)
		}
	}))

	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })

	return server, &written
}

// assertBoundedRead checks the lister stopped reading soon after the 1 MiB
// limit. The upstream can write a little more than was read: the socket
// buffers hold some.
func assertBoundedRead(t *testing.T, written *atomic.Int64) {
	t.Helper()

	assert.Less(t, written.Load(), int64(16<<20), "the tool list was read far past the 1 MiB limit")
}

func TestMCPToolListerStopsReadingAnOversizedToolList(t *testing.T) {
	server, written := hugeToolListUpstream(t)

	_, _, err := runtime.MCPToolLister(t.Context(), server.URL)
	require.ErrorIs(t, err, runtime.ErrToolListTooLarge)
	assertBoundedRead(t, written)
}

func TestStarterFailsInstanceWithOversizedToolList(t *testing.T) {
	server, written := hugeToolListUpstream(t)

	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod())
	fixture.starter.ListTools = func(ctx context.Context, _ string) ([]*mcp.Tool, string, error) {
		return runtime.MCPToolLister(ctx, server.URL)
	}

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Equal(t, toolListTooLargeReason, reason)
	assertBoundedRead(t, written)
}

func TestMonitorFailsInstanceWhenChangedToolListIsOversized(t *testing.T) {
	server, written := hugeToolListUpstream(t)

	template := testTemplate()
	instance := db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Desired: "running", Status: "ready", Namespace: instanceNamespace, Tools: reformatted(t, encodeTools(t, searchTool)),
	}
	store := &healthStore{row: instance}

	var (
		mu           sync.Mutex
		toolsChanged func()
	)

	monitor := &runtime.Monitor{
		Store: store,
		Template: func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			if key != (types.NamespacedName{Namespace: template.Namespace, Name: template.Name}) {
				return nil, false
			}

			return template.DeepCopy(), true
		},
		Connect: func(ctx context.Context, _, revision string, changed func()) (runtime.MonitorSession, error) {
			mu.Lock()
			toolsChanged = changed
			mu.Unlock()

			return runtime.MCPMonitorConnector(ctx, server.URL, revision, changed)
		},
		Interval: time.Hour,
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Run must have started before Step starts monitors.
	require.Eventually(t, func() bool {
		require.NoError(t, monitor.Step(t.Context(), instance))

		mu.Lock()
		defer mu.Unlock()

		return toolsChanged != nil
	}, 10*time.Second, time.Millisecond)

	// The upstream announces `list_changed`.
	mu.Lock()
	notify := toolsChanged
	mu.Unlock()
	notify()

	require.Eventually(t, func() bool {
		row, _ := store.snapshot()

		return row.Status == "failed"
	}, 30*time.Second, time.Millisecond)

	row, writes := store.snapshot()
	assert.Equal(t, toolListTooLargeReason, row.Reason)
	assert.Zero(t, writes)
	assertBoundedRead(t, written)
}
