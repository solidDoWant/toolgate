package runtime_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// statelessUpstream is a go-sdk server that speaks only 2026-07-28, behind
// a handler that records methods, can refuse `server/discover`, can end
// the open `subscriptions/listen` requests, and can hold later ones.
type statelessUpstream struct {
	server *mcp.Server
	url    string

	refuseDiscover atomic.Bool
	// discoverGate is held for reading by each `server/discover`.
	discoverGate sync.RWMutex

	mu        sync.Mutex
	methods   []string
	listens   int
	cutListen []context.CancelFunc
	listening sync.WaitGroup
	hold      chan struct{}
}

func newStatelessUpstream(t *testing.T) *statelessUpstream {
	t.Helper()

	upstream := &statelessUpstream{hold: make(chan struct{})}
	upstream.server = mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"},
		&mcp.ServerOptions{SupportedProtocolVersions: []string{runtime.RevisionStateless}})
	upstream.addTool("search")

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream.server },
		&mcp.StreamableHTTPOptions{Stateless: true})

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(raw))

		method := ""

		if message, err := jsonrpc.DecodeMessage(raw); err == nil {
			if request, ok := message.(*jsonrpc.Request); ok {
				method = request.Method
			}
		}

		switch method {
		case "server/discover":
			upstream.discoverGate.RLock()
			upstream.discoverGate.RUnlock()

			if upstream.refuseDiscover.Load() {
				upstream.record(method)
				w.WriteHeader(http.StatusServiceUnavailable)

				return
			}
		case "subscriptions/listen":
			ctx, cancel := context.WithCancel(r.Context())

			upstream.mu.Lock()
			upstream.listens++
			first := upstream.listens == 1
			upstream.cutListen = append(upstream.cutListen, cancel)
			upstream.mu.Unlock()

			if !first {
				// Later listen requests never reach the server, so only a
				// re-list can see a change.
				upstream.record(method)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				http.NewResponseController(w).Flush() //nolint:errcheck // A failed flush ends the stream.

				select {
				case <-upstream.hold:
				case <-ctx.Done():
				}

				return
			}

			upstream.listening.Add(1)
			defer upstream.listening.Done()

			r = r.WithContext(ctx)
		}

		upstream.record(method)
		handler.ServeHTTP(w, r)
	}))

	t.Cleanup(func() {
		close(upstream.hold)
		upstream.mu.Lock()
		for _, cancel := range upstream.cutListen {
			cancel()
		}
		upstream.mu.Unlock()
		httpServer.Close()
	})

	upstream.url = httpServer.URL

	return upstream
}

func (u *statelessUpstream) addTool(name string) {
	mcp.AddTool(u.server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
}

func (u *statelessUpstream) record(method string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.methods = append(u.methods, method)
}

func (u *statelessUpstream) seen() ([]string, int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.methods), u.listens
}

// endListen ends the open listen requests and waits until the server
// has finished them.
func (u *statelessUpstream) endListen() {
	u.mu.Lock()
	for _, cancel := range u.cutListen {
		cancel()
	}
	u.mu.Unlock()

	u.listening.Wait()
}

// monitorStateless runs a monitor with interval for a ready 2026-07-28
// instance of upstream, whose stored tools are `search`.
func monitorStateless(t *testing.T, upstream *statelessUpstream, interval time.Duration) *healthStore {
	t.Helper()

	template := testTemplate()
	instance := db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Desired: "running", Status: "ready", Namespace: instanceNamespace, McpRevision: runtime.RevisionStateless,
		Tools: encodeTools(t, &mcp.Tool{Name: "search", InputSchema: map[string]any{"type": "object"}}),
	}
	healthRows := &healthStore{row: instance}
	monitor := &runtime.Monitor{
		Store: healthRows,
		Template: func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			return template.DeepCopy(), key == types.NamespacedName{Namespace: template.Namespace, Name: template.Name}
		},
		Connect: func(ctx context.Context, _, revision string, changed func()) (runtime.MonitorSession, error) {
			return runtime.MCPMonitorConnector(ctx, upstream.url, revision, changed)
		},
		Interval: interval,
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

	require.Eventually(t, func() bool {
		require.NoError(t, monitor.Step(t.Context(), instance))

		_, listens := upstream.seen()

		return listens > 0
	}, 10*time.Second, time.Millisecond)

	return healthRows
}

func TestStatelessMonitorFailsAfterThreeFailedDiscovers(t *testing.T) {
	upstream := newStatelessUpstream(t)
	healthRows := monitorStateless(t, upstream, 20*time.Millisecond)

	upstream.refuseDiscover.Store(true)

	require.Eventually(t, func() bool {
		row, _ := healthRows.snapshot()

		return row.Status == "failed"
	}, 10*time.Second, time.Millisecond)

	row, _ := healthRows.snapshot()
	assert.Contains(t, row.Reason, "The server didn't answer 3 health checks in a row")

	methods, _ := upstream.seen()
	assert.NotContains(t, methods, "ping", "2026-07-28 has no ping")
	assert.Contains(t, methods, "server/discover")
}

func TestStatelessMonitorRelistsOnceAfterTheListenRequestEnds(t *testing.T) {
	upstream := newStatelessUpstream(t)
	healthRows := monitorStateless(t, upstream, 20*time.Millisecond)

	// The re-list that follows the first connect has run.
	require.Eventually(t, func() bool {
		methods, _ := upstream.seen()

		return slices.Contains(methods, "tools/list")
	}, 10*time.Second, time.Millisecond)

	// No health check runs between the end of the listen request and the
	// tool change, so the change is made while nothing listens.
	upstream.discoverGate.Lock()
	upstream.endListen()
	upstream.addTool("two")
	upstream.discoverGate.Unlock()

	require.Eventually(t, func() bool {
		row, _ := healthRows.snapshot()

		return bytes.Contains(row.Tools, []byte(`"two"`))
	}, 10*time.Second, time.Millisecond, "the monitor re-lists after connecting again")

	row, _ := healthRows.snapshot()
	assert.Equal(t, "ready", row.Status)

	_, listens := upstream.seen()
	assert.GreaterOrEqual(t, listens, 2, "the monitor connects again with a new listen request")
}
