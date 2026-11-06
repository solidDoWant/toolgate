//go:build integration

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// upstreamServer is a real MCP server standing in for an instance.
type upstreamServer struct {
	// started receives once each time the slow tool starts.
	started chan struct{}
	// release lets every slow call return.
	release chan struct{}

	mu sync.Mutex
	// called counts the tool calls received, by tool name.
	called map[string]int
}

// callCount returns how many calls of the tool the upstream received.
func (u *upstreamServer) callCount(tool string) int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.called[tool]
}

// startUpstream serves the tools echo, sized, slow and fails over
// Streamable HTTP, and makes the fixture forward calls of server's
// instances to it.
func (f *mcpFixture) startUpstream(t *testing.T, server string) *upstreamServer {
	t.Helper()

	upstream := &upstreamServer{started: make(chan struct{}, 8), release: make(chan struct{}), called: map[string]int{}}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "0"}, nil)
	schema := map[string]any{"type": "object"}

	mcpServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if call, ok := request.(*mcp.CallToolRequest); ok && method == "tools/call" {
				upstream.mu.Lock()
				upstream.called[call.Params.Name]++
				upstream.mu.Unlock()
			}

			return next(ctx, method, request)
		}
	})

	mcpServer.AddTool(&mcp.Tool{Name: "echo", InputSchema: schema}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments struct {
			Text string `json:"text"`
		}

		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arguments.Text}}}, nil
	})

	mcpServer.AddTool(&mcp.Tool{Name: "sized", InputSchema: schema}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments struct {
			Size int `json:"size"`
		}

		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}

		return sizedResult(arguments.Size), nil
	})

	mcpServer.AddTool(&mcp.Tool{Name: "slow", InputSchema: schema}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		upstream.started <- struct{}{}

		select {
		case <-upstream.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "slow done"}}}, nil
	})

	// raw returns the arguments' bytes as the upstream received them.
	mcpServer.AddTool(&mcp.Tool{Name: "raw", InputSchema: schema}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(request.Params.Arguments)}}}, nil
	})

	mcpServer.AddTool(&mcp.Tool{Name: "fails", InputSchema: schema}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "upstream says no"}}}, nil
	})

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))

	t.Cleanup(func() {
		// A slow call still running must end before Close can return.
		select {
		case <-upstream.release:
		default:
			close(upstream.release)
		}

		// The gateway's forwarding session holds a stream open.
		httpServer.CloseClientConnections()
		httpServer.Close()
	})

	f.upstreams.Store(server, httpServer.URL)

	return upstream
}

// sizedResult is a tool result whose JSON encoding is exactly size bytes
// (for any size above the encoding of an empty text result).
func sizedResult(size int) *mcp.CallToolResult {
	empty, _ := json.Marshal(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: ""}}})

	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", size-len(empty))}}}
}

// enableWithTools enables server in client's session and makes its instance
// ready with tools.
func (f *mcpFixture) enableWithTools(t *testing.T, client *observedClient, server, scopeKey string, tools ...string) db.Instance {
	t.Helper()

	results := startEnable(t, client.session, server)
	instance := f.awaitInstance(t, f.catalogTemplate(t, server), scopeKey)
	f.markReady(t, instance.ID, tools...)
	require.Empty(t, awaitEnable(t, results).err)

	return instance
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)

	return result
}

func resultText(result *mcp.CallToolResult) string {
	text := ""

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}

	return text
}

// auditRow is one audit_events row with its identity columns.
type auditRow struct {
	Principal, Workspace, Session, Attestation, Server, Tool, TraceID string
	Details                                                           map[string]any
}

func (f *mcpFixture) auditRows(t *testing.T, eventType string) []auditRow {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), `SELECT principal, workspace, session, attestation, server, tool, trace_id, details
		FROM audit_events WHERE type = $1 ORDER BY id`, eventType)
	require.NoError(t, err)

	collected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditRow, error) {
		var event auditRow

		err := row.Scan(&event.Principal, &event.Workspace, &event.Session, &event.Attestation,
			&event.Server, &event.Tool, &event.TraceID, &event.Details)

		return event, err
	})
	require.NoError(t, err)

	return collected
}

func TestCallForwarding(t *testing.T) {
	t.Run("[AC-1-08] a call returns the upstream result and writes tool.decision and tool.result with full identity", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		caller := alice()
		caller.workspace = "laptop-alice"
		client := fixture.connectObserved(t, caller)
		instance := fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "hello from upstream"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "hello from upstream", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		results := fixture.auditRows(t, "tool.result")
		require.Len(t, decisions, 1)
		require.Len(t, results, 1)

		for _, event := range []auditRow{decisions[0], results[0]} {
			assert.Equal(t, "alice", event.Principal)
			assert.Equal(t, "claimed/laptop-alice", event.Workspace)
			assert.Equal(t, "session-1", event.Session)
			assert.Equal(t, auth.AttestationUser, event.Attestation)
			assert.Equal(t, "github", event.Server)
			assert.Equal(t, "echo", event.Tool)
			assert.Len(t, event.TraceID, 32)
		}

		assert.Equal(t, "allow", decisions[0].Details["decision"])
		assert.Equal(t, "ok", results[0].Details["outcome"])
		assert.Equal(t, instance.ID, results[0].Details["instance"])

		row, err := db.New(fixture.pool).GetInstance(t.Context(), instance.ID)
		require.NoError(t, err)
		assert.True(t, row.LastUsedAt.Valid, "last_used_at is set")
	})

	t.Run("a tool error from upstream is returned as is and recorded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "fails")

		result := callTool(t, client.session, "github__fails", nil)
		assert.True(t, result.IsError)
		assert.Equal(t, "upstream says no", resultText(result))
		assert.Equal(t, "error", fixture.auditRows(t, "tool.result")[0].Details["outcome"])
	})

	t.Run("a call of an allowed server that isn't enabled enables it with first-call and is forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		// Another session of alice has not enabled the server; its call
		// enables it.
		secondUser := alice()
		secondUser.session = "session-2"
		second := fixture.connectObserved(t, secondUser)

		result := callTool(t, second.session, "github__echo", map[string]any{"text": "x"})
		require.False(t, result.IsError, resultText(result))

		enables := fixture.auditRows(t, "server.enable")
		require.NotEmpty(t, enables)
		last := enables[len(enables)-1]
		assert.Equal(t, "first-call", last.Details["reason"])
		assert.Equal(t, "session-2", last.Session)
		assert.Equal(t, "allow", last.Details["decision"])
		require.Eventually(t, func() bool { return second.listChangedCount() > 0 }, waitTimeout, 10*time.Millisecond)
	})

	t.Run("a call of a server the policy refuses is refused and audited without enabling it", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})
		fixture.startUpstream(t, "github")

		client := fixture.connect(t, alice())

		result := callTool(t, client, "github__echo", map[string]any{"text": "x"})
		assert.True(t, result.IsError)

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Empty(t, fixture.auditRows(t, "server.enable"))
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
	})

	t.Run("a call whose instance stopped being ready is refused with not-ready and its reason", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			// Fail it without notifying, so the session still offers the tool.
			_, err := db.New(tx).SetInstanceFailed(t.Context(), db.SetInstanceFailedParams{ID: instance.ID, Reason: "health checks failed", FromStatus: "ready"})

			return err
		}))

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), "health checks failed")

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, "not-ready", decisions[0].Details["reason"])
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
	})

	t.Run("a call the policy refuses is audited and never forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "slow")

		require.NoError(t, fixture.rules.Set(blockServer("github")))

		result := callTool(t, client.session, "github__slow", nil)
		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), "GitHub is frozen")

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
		assert.Empty(t, upstream.started, "the call reached the upstream server")
	})
}

func TestCallLimits(t *testing.T) {
	t.Run("a result of exactly 16 MiB is forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "sized")

		result := callTool(t, client.session, "github__sized", map[string]any{"size": 16 << 20})
		require.False(t, result.IsError, resultText(result)[:min(200, len(resultText(result)))])

		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		assert.Len(t, encoded, 16<<20)
	})

	t.Run("a result one byte over 16 MiB is refused with a clear error", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "sized")

		result := callTool(t, client.session, "github__sized", map[string]any{"size": 16<<20 + 1})
		assert.True(t, result.IsError)
		assert.Equal(t, "The result of `github__sized` is larger than 16 MiB, the limit, so the gateway didn't forward it.", resultText(result))
		assert.Equal(t, "too-large", fixture.auditRows(t, "tool.result")[0].Details["outcome"])
	})

	t.Run("a request body over 4 MiB is refused with a clear error", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		response := fixture.post(t, alice(), client.session.ID(), map[string]any{
			"jsonrpc": "2.0", "id": 7, "method": "tools/call",
			"params": map[string]any{"name": "github__echo", "arguments": map[string]any{"text": strings.Repeat("a", 4<<20)}},
		})
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)

		assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, string(body))
		assert.Contains(t, string(body), "4 MiB")
		assert.Empty(t, fixture.auditRows(t, "tool.decision"))
	})

	t.Run("a request body just under 4 MiB is forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		message := map[string]any{
			"jsonrpc": "2.0", "id": 7, "method": "tools/call",
			"params": map[string]any{"name": "github__echo", "arguments": map[string]any{"text": ""}},
		}
		empty, err := json.Marshal(message)
		require.NoError(t, err)

		message["params"].(map[string]any)["arguments"] = map[string]any{"text": strings.Repeat("a", 4<<20-len(empty))}
		encoded, err := json.Marshal(message)
		require.NoError(t, err)
		require.Len(t, encoded, 4<<20)

		response := fixture.post(t, alice(), client.session.ID(), message)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, response.StatusCode)
		assert.True(t, bytes.Contains(body, []byte(`"isError":false`)) || !bytes.Contains(body, []byte(`"isError":true`)), "the call failed")
		assert.Len(t, fixture.auditRows(t, "tool.result"), 1)
	})
}

func TestCallInteractions(t *testing.T) {
	t.Run("row 4: a disable while a call is in flight lets the call finish, refuses later calls with not-enabled and withdraws the tools", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "slow")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		calls := make(chan *mcp.CallToolResult, 1)

		go func() {
			result, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "github__slow"})
			if err != nil {
				result = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
			}

			calls <- result
		}()

		select {
		case <-upstream.started:
		case <-time.After(waitTimeout):
			require.FailNow(t, "the call never reached the upstream server")
		}

		// The disable comes from another MCP session of the same toolgate
		// session, so only fan-out can tell the calling session.
		other := fixture.connectObserved(t, alice())
		disable(t, other.session, "github")

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)

		close(upstream.release)

		select {
		case result := <-calls:
			require.False(t, result.IsError, resultText(result))
			assert.Equal(t, "slow done", resultText(result))
		case <-time.After(waitTimeout):
			require.FailNow(t, "the in-flight call did not finish")
		}

		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		// A later call enables the server again (docs/design/phase-4.md §4).
		later := callTool(t, client.session, "github__slow", nil)
		require.False(t, later.IsError, resultText(later))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 2)
		assert.Equal(t, "allow", decisions[0].Details["decision"])
		assert.Equal(t, "allow", decisions[1].Details["decision"])
	})

	t.Run("row 20: an upstream tool change sends list_changed with the new list to sessions using the server", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.addEnableTemplate("jira", "per-user", anyone)

		client := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, client, "github", "user:alice", "search")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		bystander := fixture.connectObserved(t, bob())
		fixture.enableWithTools(t, bystander, "jira", "user:bob", "search")
		require.Eventually(t, func() bool { return bystander.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		// What the reconciler does after the upstream announces
		// list_changed: store the new list and notify.
		tools, err := json.Marshal([]*mcp.Tool{
			{Name: "search", InputSchema: map[string]any{"type": "object"}},
			{Name: "create_issue", InputSchema: map[string]any{"type": "object"}},
		})
		require.NoError(t, err)

		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			if _, err := db.New(tx).SetInstanceTools(t.Context(), db.SetInstanceToolsParams{ID: instance.ID, Tools: tools}); err != nil {
				return err
			}

			return store.Notify(t.Context(), tx, store.KindInstance, instance.ID)
		}))

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, append(metaToolNames, "github__search", "github__create_issue"), toolNames(t, client.session))
		assert.Equal(t, 1, bystander.listChangedCount(), "a session not using the server got list_changed")
	})
}
