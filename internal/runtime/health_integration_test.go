//go:build integration

package runtime_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

type healthHarness struct {
	pool     *pgxpool.Pool
	queries  *db.Queries
	events   <-chan store.Event
	upstream *mcp.Server
	http     *httptest.Server
	session  db.Session
}

// newHealthHarness starts PostgreSQL with a ready instance that a session
// has enabled, a real MCP upstream with one tool, and a monitor for the
// instance that reaches the upstream.
func newHealthHarness(t *testing.T) *healthHarness {
	t.Helper()

	pg := testutil.StartPostgres(t)

	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	harness := &healthHarness{pool: pool, queries: db.New(pool)}

	harness.upstream = mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "0"}, nil)
	addTool(harness.upstream, "search")
	harness.http = httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return harness.upstream }, nil))
	t.Cleanup(harness.http.Close)

	harness.session, err = harness.queries.CreateSession(t.Context(), db.CreateSessionParams{
		Principal: "alice", SessionID: "session-1", Workspace: "laptop", Attestation: "user", Kind: "user", Groups: []string{},
	})
	require.NoError(t, err)
	require.NoError(t, harness.queries.EnableServer(t.Context(), db.EnableServerParams{
		SessionPk: harness.session.ID, TemplateNs: "catalog", TemplateName: "github",
	}))

	_, err = harness.queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: instanceID, TemplateNs: "catalog", TemplateName: "github", TemplateUid: templateUID,
		Generation: 1, Scope: "per-user", ScopeKey: "user:alice", OwnerPrincipal: "alice", Namespace: instanceNamespace,
	})
	require.NoError(t, err)

	startStore := runtime.PostgresStartStore{Pool: pool}
	_, err = startStore.MarkStarting(t.Context(), instanceID, 1, nil, time.Time{})
	require.NoError(t, err)

	tools, revision, err := runtime.MCPToolLister(t.Context(), harness.http.URL)
	require.NoError(t, err)

	encoded, reason := runtime.CheckToolList("github", tools)
	require.Empty(t, reason)
	_, err = startStore.MarkReady(t.Context(), db.Instance{ID: instanceID, TemplateName: "github"}, encoded, revision)
	require.NoError(t, err)

	listener := store.NewListener(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	events, unsubscribe := listener.Subscribe()
	t.Cleanup(unsubscribe)

	harness.events = events

	ctx, cancel := context.WithCancel(t.Context())

	go func() { _ = listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	template := testTemplate()
	monitor := &runtime.Monitor{
		Store: startStore,
		Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			return template.DeepCopy(), true
		},
		// The instance endpoint is a cluster DNS name; reach the local
		// upstream instead.
		Connect: func(ctx context.Context, _, revision string, toolsChanged func()) (runtime.MonitorSession, error) {
			return runtime.MCPMonitorConnector(ctx, harness.http.URL, revision, toolsChanged)
		},
		Interval: 100 * time.Millisecond,
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	pass := runtime.FullPass(harness.queries, slog.New(slog.NewTextHandler(os.Stderr, nil)), monitor.Step)

	require.Eventually(t, func() bool {
		require.NoError(t, pass(t.Context()))

		return harness.upstreamSessions() > 0
	}, 30*time.Second, 10*time.Millisecond, "the monitor opens a session")

	return harness
}

func addTool(server *mcp.Server, name string) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: name + "."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
}

func (h *healthHarness) upstreamSessions() int {
	count := 0
	for range h.upstream.Sessions() {
		count++
	}

	return count
}

func (h *healthHarness) waitForInstanceEvent(t *testing.T) {
	t.Helper()

	deadline := time.After(30 * time.Second)

	for {
		select {
		case event := <-h.events:
			if event.Missed {
				// Sent when the listener connects.
				continue
			}

			assert.Equal(t, store.Event{Kind: store.KindInstance, ID: instanceID}, event)

			return
		case <-deadline:
			t.Fatal("no notification within 30s")
		}
	}
}

// Row 20: the upstream announces `list_changed`. The reconciler re-lists,
// checks and stores the tools, and notifies `instance:<id>`.
func TestMonitorUpstreamToolListChanged(t *testing.T) {
	harness := newHealthHarness(t)

	addTool(harness.upstream, "fetch")

	harness.waitForInstanceEvent(t)

	row, err := harness.queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, "ready", row.Status)

	var stored []map[string]any
	require.NoError(t, json.Unmarshal(row.Tools, &stored))

	names := make([]string, 0, len(stored))
	for _, tool := range stored {
		names = append(names, tool["name"].(string))
	}

	assert.ElementsMatch(t, []string{"search", "fetch"}, names)
}

// Row 12, the reconciler's part: an instance that stops answering after
// ready fails after 3 health checks in a row, with a reason, and notifies.
// The session keeps the server enabled.
func TestMonitorInstanceFailsAfterReady(t *testing.T) {
	harness := newHealthHarness(t)

	harness.http.CloseClientConnections()
	harness.http.Close()

	harness.waitForInstanceEvent(t)

	row, err := harness.queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, "failed", row.Status)
	assert.Equal(t, "stopped", row.Desired)
	assert.True(t, row.FailedAt.Valid)
	assert.Contains(t, row.Reason, "The server didn't answer 3 health checks in a row")

	enabled, err := harness.queries.ListSessionServers(t.Context(), harness.session.ID)
	require.NoError(t, err)
	require.Len(t, enabled, 1, "the session keeps the server enabled")
	assert.Equal(t, "github", enabled[0].TemplateName)
}
