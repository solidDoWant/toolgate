//go:build integration

package gateway_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// sessionfulRevision is an MCP revision with upstream sessions.
const sessionfulRevision = "2025-11-25"

// sessionUpstream is an upstream MCP server that records the initialize
// requests it gets and the session IDs of the DELETEs.
type sessionUpstream struct {
	url string

	mu          sync.Mutex
	initializes int
	deleted     []string
	// holdInitialize, when set, holds each initialize until it is closed.
	holdInitialize chan struct{}
	// initializing receives once each time an initialize arrives.
	initializing chan struct{}
	// forgotten holds the session IDs the server answers 404 for, as after a
	// restart, and forgetCalls makes it answer 404 to every tools/call
	// that carries a session ID.
	forgotten   map[string]bool
	forgetCalls bool
	// notFoundBody is the body of each 404; empty means plain text.
	notFoundBody string
}

// forget makes the server answer 404 to every later request of the
// upstream session id.
func (u *sessionUpstream) forget(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.forgotten[id] = true
}

// lostSession reports whether the server answers request with 404 for its
// session.
func (u *sessionUpstream) lostSession(request *http.Request) bool {
	id := request.Header.Get("Mcp-Session-Id")
	if id == "" {
		return false
	}

	u.mu.Lock()
	forgotten, forgetCalls := u.forgotten[id], u.forgetCalls
	u.mu.Unlock()

	if forgotten {
		return true
	}

	if !forgetCalls || request.Method != http.MethodPost {
		return false
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		return false
	}

	request.Body = io.NopCloser(bytes.NewReader(body))

	return bytes.Contains(body, []byte(`"tools/call"`))
}

func (u *sessionUpstream) initializeCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.initializes
}

func (u *sessionUpstream) deletes() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]string(nil), u.deleted...)
}

// sessionUpstreamOptions configures startSessionUpstream.
type sessionUpstreamOptions struct {
	// sessionID makes the server's session IDs; nil means random ones, and
	// an empty ID makes the server sessionless.
	sessionID func() string
	// name is the server's implementation name.
	name string
	// hold holds each initialize until the test releases it.
	hold bool
	// instructions are the server's instructions.
	instructions string
	// stateless makes the server speak only 2026-07-28.
	stateless bool
}

// startSessionUpstream serves the tool session_id, which returns the
// upstream session ID of the call, and makes the fixture forward server's
// calls to it.
func (f *mcpFixture) startSessionUpstream(t *testing.T, server string, options sessionUpstreamOptions) *sessionUpstream {
	t.Helper()

	upstream := &sessionUpstream{initializing: make(chan struct{}, 16), forgotten: make(map[string]bool)}
	if options.hold {
		upstream.holdInitialize = make(chan struct{})
	}

	name := options.name
	if name == "" {
		name = "upstream"
	}

	serverOptions := &mcp.ServerOptions{GetSessionID: options.sessionID, Instructions: options.instructions}
	httpOptions := &mcp.StreamableHTTPOptions{}

	if options.stateless {
		serverOptions.SupportedProtocolVersions = []string{runtime.RevisionStateless}
		httpOptions.Stateless = true
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0"}, serverOptions)

	mcpServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				upstream.mu.Lock()
				upstream.initializes++
				upstream.mu.Unlock()

				upstream.initializing <- struct{}{}

				if upstream.holdInitialize != nil {
					select {
					case <-upstream.holdInitialize:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}

			return next(ctx, method, request)
		}
	})

	mcpServer.AddTool(&mcp.Tool{Name: "session_id", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "session:" + request.Session.ID()}}}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, httpOptions)

	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			upstream.mu.Lock()
			upstream.deleted = append(upstream.deleted, request.Header.Get("Mcp-Session-Id"))
			upstream.mu.Unlock()
		}

		if upstream.lostSession(request) {
			upstream.mu.Lock()
			body := upstream.notFoundBody
			upstream.mu.Unlock()

			if body == "" {
				http.Error(writer, "session not found", http.StatusNotFound)

				return
			}

			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(body))

			return
		}

		handler.ServeHTTP(writer, request)
	}))

	t.Cleanup(func() {
		if upstream.holdInitialize != nil {
			select {
			case <-upstream.holdInitialize:
			default:
				close(upstream.holdInitialize)
			}
		}

		httpServer.CloseClientConnections()
		httpServer.Close()
	})

	upstream.url = httpServer.URL
	f.upstreams.Store(server, httpServer.URL)

	return upstream
}

// awaitInitialize waits until the upstream got an initialize.
func (u *sessionUpstream) awaitInitialize(t *testing.T) {
	t.Helper()

	select {
	case <-u.initializing:
	case <-time.After(waitTimeout):
		require.FailNow(t, "no initialize reached the upstream")
	}
}

// release lets every held initialize go on.
func (u *sessionUpstream) release() {
	close(u.holdInitialize)
}

// setRevision sets the MCP revision an instance speaks.
func (f *mcpFixture) setRevision(t *testing.T, id, revision string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), "UPDATE instances SET mcp_revision = $2 WHERE id = $1", id, revision)
	require.NoError(t, err)
}

// upstreamSessionRow is one upstream_sessions row.
type upstreamSessionRow struct {
	instance  string
	sessionPk int64
	upstream  string
	result    string
	live      bool
}

func (f *mcpFixture) upstreamSessionRows(t *testing.T) []upstreamSessionRow {
	t.Helper()

	rows, err := f.pool.Query(t.Context(),
		"SELECT instance_id, session_pk, upstream_session_id, initialize_result::text, ended_at IS NULL FROM upstream_sessions ORDER BY id")
	require.NoError(t, err)

	defer rows.Close()

	var collected []upstreamSessionRow

	for rows.Next() {
		var row upstreamSessionRow
		require.NoError(t, rows.Scan(&row.instance, &row.sessionPk, &row.upstream, &row.result, &row.live))
		collected = append(collected, row)
	}

	require.NoError(t, rows.Err())

	return collected
}

func liveRows(rows []upstreamSessionRow) []upstreamSessionRow {
	var live []upstreamSessionRow

	for _, row := range rows {
		if row.live {
			live = append(live, row)
		}
	}

	return live
}

// callSessionID calls the session_id tool of server and returns the
// upstream session ID the server saw.
func callSessionID(t *testing.T, session *mcp.ClientSession, server string) string {
	t.Helper()

	result := callTool(t, session, server+"__session_id", nil)
	require.False(t, result.IsError, resultText(result))

	text := resultText(result)
	require.True(t, strings.HasPrefix(text, "session:"), text)

	return strings.TrimPrefix(text, "session:")
}

// startSessionIDCall calls session_id in the background.
func startSessionIDCall(ctx context.Context, session *mcp.ClientSession, server string) <-chan *mcp.CallToolResult {
	results := make(chan *mcp.CallToolResult, 1)

	go func() {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: server + "__session_id"})
		if err != nil {
			result = sessionCallError(err.Error())
		}

		results <- result
	}()

	return results
}

func sessionCallError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func awaitSessionIDCall(t *testing.T, results <-chan *mcp.CallToolResult) *mcp.CallToolResult {
	t.Helper()

	select {
	case result := <-results:
		return result
	case <-time.After(waitTimeout):
		require.FailNow(t, "the call didn't finish")

		return nil
	}
}

// awaitDecisions waits until count tool.decision rows are committed, so the
// calls have reached the forward.
func (f *mcpFixture) awaitDecisions(t *testing.T, count int) {
	t.Helper()

	require.Eventually(t, func() bool { return len(f.auditRows(t, "tool.decision")) >= count }, waitTimeout, 10*time.Millisecond)
}

// sessionfulInstance enables server in client's session and makes its
// instance ready on a revision with sessions.
func (f *mcpFixture) sessionfulInstance(t *testing.T, client *observedClient, server, scopeKey string) string {
	t.Helper()

	instance := f.enableWithTools(t, client, server, scopeKey, "session_id")
	f.setRevision(t, instance.ID, sessionfulRevision)

	return instance.ID
}

func TestForwardUsesOneUpstreamSessionAcrossReplicas(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})
	other := fixture.replica(t)

	first := fixture.connectObserved(t, alice())
	instanceID := fixture.sessionfulInstance(t, first, "github", "user:alice")
	second := fixture.connectObservedTo(t, other.URL, alice())

	id := callSessionID(t, first.session, "github")
	assert.Equal(t, id, callSessionID(t, second.session, "github"), "the other replica adopts the stored session")

	// Two calls of the pair at once run on one copy.
	calls := []<-chan *mcp.CallToolResult{startSessionIDCall(t.Context(), second.session, "github"), startSessionIDCall(t.Context(), second.session, "github")}
	for _, call := range calls {
		result := awaitSessionIDCall(t, call)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "session:"+id, resultText(result))
	}

	assert.Equal(t, 1, upstream.initializeCount(), "only the first call sends initialize")
	assert.Empty(t, upstream.deletes())

	rows := fixture.upstreamSessionRows(t)
	require.Len(t, rows, 1)
	assert.Equal(t, instanceID, rows[0].instance)
	assert.Equal(t, fixture.sessionRow(t, "alice", "session-1").ID, rows[0].sessionPk)
	assert.Equal(t, id, rows[0].upstream)
	assert.True(t, rows[0].live)
}

func TestEachToolgateSessionGetsItsOwnUpstreamSession(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "shared", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "shared")

	second := fixture.connectObserved(t, bob())
	require.Empty(t, awaitEnable(t, startEnable(t, second.session, "github")).err)

	aliceID := callSessionID(t, first.session, "github")
	bobID := callSessionID(t, second.session, "github")

	assert.NotEqual(t, aliceID, bobID, "two toolgate sessions never share an upstream session")
	assert.Equal(t, aliceID, callSessionID(t, first.session, "github"))
	assert.Equal(t, 2, upstream.initializeCount())
	assert.Len(t, liveRows(fixture.upstreamSessionRows(t)), 2)
}

func TestFirstForwardRaceKeepsOneUpstreamSession(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{hold: true})
	other := fixture.replica(t)

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "user:alice")
	second := fixture.connectObservedTo(t, other.URL, alice())

	// Both replicas send initialize before either claims the pair.
	calls := []<-chan *mcp.CallToolResult{startSessionIDCall(t.Context(), first.session, "github"), startSessionIDCall(t.Context(), second.session, "github")}
	upstream.awaitInitialize(t)
	upstream.awaitInitialize(t)
	upstream.release()

	var seen []string

	for _, call := range calls {
		result := awaitSessionIDCall(t, call)
		require.False(t, result.IsError, resultText(result))
		seen = append(seen, resultText(result))
	}

	assert.Equal(t, seen[0], seen[1], "both calls use the winner's upstream session")

	live := liveRows(fixture.upstreamSessionRows(t))
	require.Len(t, live, 1)
	assert.Equal(t, "session:"+live[0].upstream, seen[0])

	require.Eventually(t, func() bool { return len(upstream.deletes()) == 1 }, waitTimeout, 10*time.Millisecond)
	assert.NotEqual(t, live[0].upstream, upstream.deletes()[0], "the loser ends its own upstream session")
}

func TestConcurrentFirstForwardsOnOneReplicaInitializeOnce(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{hold: true})

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "user:alice")
	second := fixture.connectObserved(t, alice())

	calls := []<-chan *mcp.CallToolResult{startSessionIDCall(t.Context(), first.session, "github")}
	upstream.awaitInitialize(t)

	calls = append(calls, startSessionIDCall(t.Context(), second.session, "github"))
	fixture.awaitDecisions(t, 2)
	upstream.release()

	var seen []string

	for _, call := range calls {
		result := awaitSessionIDCall(t, call)
		require.False(t, result.IsError, resultText(result))
		seen = append(seen, resultText(result))
	}

	assert.Equal(t, seen[0], seen[1])
	assert.Equal(t, 1, upstream.initializeCount(), "the second forward waits for the first one's row")
	assert.Empty(t, upstream.deletes())
	assert.Len(t, liveRows(fixture.upstreamSessionRows(t)), 1)
}

func TestDisableEndsUpstreamSession(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "user:alice")
	id := callSessionID(t, client.session, "github")

	disable(t, client.session, "github")

	rows := fixture.upstreamSessionRows(t)
	require.Len(t, rows, 1)
	assert.Equal(t, id, rows[0].upstream)
	assert.False(t, rows[0].live, "the disable ends the pair's row")
}

func TestEnableAfterDisableStartsNewUpstreamSession(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "shared", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "shared")
	before := callSessionID(t, client.session, "github")

	disable(t, client.session, "github")
	require.Empty(t, awaitEnable(t, startEnable(t, client.session, "github")).err)

	after := callSessionID(t, client.session, "github")
	assert.NotEqual(t, before, after)
	assert.Equal(t, 2, upstream.initializeCount())

	live := liveRows(fixture.upstreamSessionRows(t))
	require.Len(t, live, 1)
	assert.Equal(t, after, live[0].upstream)
}

func TestDisableDuringFirstForwardLeavesNoLiveRow(t *testing.T) {
	for name, end := range map[string]func(t *testing.T, fixture *mcpFixture, client *observedClient){
		"a disable commits while initialize is in flight": func(t *testing.T, _ *mcpFixture, client *observedClient) {
			disable(t, client.session, "github")
		},
		"a session end commits while initialize is in flight": func(t *testing.T, fixture *mcpFixture, _ *observedClient) {
			require.NoError(t, runtime.EndSession(t.Context(), fixture.pool, nil, fixture.sessionRow(t, "alice", "session-1").ID, "closed"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newMCPFixture(t)
			fixture.addEnableTemplate("github", "shared", anyone)
			upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{hold: true})

			client := fixture.connectObserved(t, alice())
			fixture.sessionfulInstance(t, client, "github", "shared")
			other := fixture.connectObserved(t, alice())

			call := startSessionIDCall(t.Context(), client.session, "github")
			upstream.awaitInitialize(t)

			end(t, fixture, other)
			upstream.release()

			result := awaitSessionIDCall(t, call)
			require.False(t, result.IsError, resultText(result))

			assert.Empty(t, fixture.upstreamSessionRows(t), "the claim stores nothing for an ended pair")
			require.Eventually(t, func() bool { return len(upstream.deletes()) == 1 }, waitTimeout, 10*time.Millisecond)
			assert.Equal(t, "session:"+upstream.deletes()[0], resultText(result), "the call ran on the new session, which then ended")
		})
	}
}

func TestSessionEndEndsItsUpstreamSessions(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	fixture.addEnableTemplate("jira", "per-user", anyone)
	fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})
	fixture.startSessionUpstream(t, "jira", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "user:alice")
	fixture.sessionfulInstance(t, client, "jira", "user:alice")
	callSessionID(t, client.session, "github")
	callSessionID(t, client.session, "jira")
	require.Len(t, liveRows(fixture.upstreamSessionRows(t)), 2)

	require.NoError(t, runtime.EndSession(t.Context(), fixture.pool, nil, fixture.sessionRow(t, "alice", "session-1").ID, "closed"))

	rows := fixture.upstreamSessionRows(t)
	assert.Len(t, rows, 2)
	assert.Empty(t, liveRows(rows), "the session end ends every live row of the session")
}

func TestResumedSessionGetsNewUpstreamSession(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "shared", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "shared")
	before := callSessionID(t, first.session, "github")

	require.NoError(t, runtime.EndSession(t.Context(), fixture.pool, nil, fixture.sessionRow(t, "alice", "session-1").ID, "closed"))

	resumed := fixture.connectObserved(t, alice())
	after := callSessionID(t, resumed.session, "github")

	assert.NotEqual(t, before, after)
	assert.Equal(t, 2, upstream.initializeCount())

	live := liveRows(fixture.upstreamSessionRows(t))
	require.Len(t, live, 1)
	assert.Equal(t, after, live[0].upstream)
}

func TestPerSessionInstanceHasOneUpstreamSession(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-session", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "session:alice:session-1")

	id := callSessionID(t, client.session, "github")
	assert.Equal(t, id, callSessionID(t, client.session, "github"))
	assert.Equal(t, 1, upstream.initializeCount())
	assert.Len(t, fixture.upstreamSessionRows(t), 1)
}

func TestSessionlessUpstreamStoresNothing(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "shared", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{sessionID: func() string { return "" }})

	first := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, first, "github", "shared")

	second := fixture.connectObserved(t, bob())
	require.Empty(t, awaitEnable(t, startEnable(t, second.session, "github")).err)

	assert.Empty(t, callSessionID(t, first.session, "github"))
	assert.Empty(t, callSessionID(t, second.session, "github"))
	assert.Equal(t, 1, upstream.initializeCount(), "the second pair uses the instance's one copy")
	assert.Empty(t, fixture.upstreamSessionRows(t))
}

func TestStatelessRevisionUpstreamStoresNothing(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "shared", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{stateless: true})

	first := fixture.connectObserved(t, alice())
	instance := fixture.enableWithTools(t, first, "github", "shared", "session_id")
	fixture.setRevision(t, instance.ID, runtime.RevisionStateless)

	second := fixture.connectObserved(t, bob())
	require.Empty(t, awaitEnable(t, startEnable(t, second.session, "github")).err)

	// The forward of a 2026-07-28 instance never reads the table.
	_, err := fixture.pool.Exec(t.Context(), "ALTER TABLE upstream_sessions RENAME TO upstream_sessions_hidden")
	require.NoError(t, err)

	for _, client := range []*observedClient{first, second} {
		result := callTool(t, client.session, "github__session_id", nil)
		require.False(t, result.IsError, resultText(result))
	}

	_, err = fixture.pool.Exec(t.Context(), "ALTER TABLE upstream_sessions_hidden RENAME TO upstream_sessions")
	require.NoError(t, err)

	assert.Zero(t, upstream.initializeCount(), "2026-07-28 has no initialize")
	assert.Empty(t, fixture.upstreamSessionRows(t))
}

// invalidIDMarker contains non-hex letters, so it can't appear by chance in a
// logged UUID the way a word such as "bad" can.
const invalidIDMarker = "zqxbad"

func TestInvalidUpstreamSessionIDFailsTheCall(t *testing.T) {
	for name, test := range map[string]struct {
		options sessionUpstreamOptions
		deleted bool
	}{
		"an ID of 1025 bytes":              {options: sessionUpstreamOptions{sessionID: func() string { return invalidIDMarker + strings.Repeat("a", 1025-len(invalidIDMarker)) }}, deleted: true},
		"an ID with a space":               {options: sessionUpstreamOptions{sessionID: func() string { return invalidIDMarker + " id" }}, deleted: true},
		"an ID with a byte above 0x7E":     {options: sessionUpstreamOptions{sessionID: func() string { return invalidIDMarker + "-\u00e9-id" }}, deleted: true},
		"an initialize result over 16 KiB": {options: sessionUpstreamOptions{name: strings.Repeat("n", 16<<10)}, deleted: true},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			fixture := newMCPFixture(t, func(options *gateway.MCPOptions) {
				options.Logger = slog.New(slog.NewJSONHandler(logs, nil))
			})
			fixture.addEnableTemplate("github", "per-user", anyone)
			upstream := fixture.startSessionUpstream(t, "github", test.options)

			client := fixture.connectObserved(t, alice())
			instanceID := fixture.sessionfulInstance(t, client, "github", "user:alice")

			result := callTool(t, client.session, "github__session_id", nil)
			assert.True(t, result.IsError)
			assert.Equal(t, "Server `github` can't be reached. Try again, or call `toolgate_server_status` to see its status.", resultText(result))
			assert.Empty(t, fixture.upstreamSessionRows(t))
			assert.Contains(t, logs.String(), `"instance":"`+instanceID+`"`, "a warning names the instance")
			assert.NotContains(t, logs.String(), invalidIDMarker, "no log line has the upstream session ID")

			if test.deleted {
				require.Eventually(t, func() bool { return len(upstream.deletes()) == 1 }, waitTimeout, 10*time.Millisecond,
					"the new upstream session is ended")
			}
		})
	}

	t.Run("an ID of 1024 visible bytes is stored", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{
			sessionID: func() string { return "!~" + strings.Repeat("a", 1022) },
		})

		client := fixture.connectObserved(t, alice())
		fixture.sessionfulInstance(t, client, "github", "user:alice")

		callSessionID(t, client.session, "github")
		assert.Len(t, fixture.upstreamSessionRows(t), 1)
	})

	t.Run("instructions are not stored and don't count toward the limit", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{
			name: strings.Repeat("n", 15<<10), instructions: strings.Repeat("i", 2<<10),
		})

		client := fixture.connectObserved(t, alice())
		fixture.sessionfulInstance(t, client, "github", "user:alice")

		callSessionID(t, client.session, "github")

		rows := fixture.upstreamSessionRows(t)
		require.Len(t, rows, 1)
		assert.NotContains(t, rows[0].result, "instructions")
	})
}

// addFailingClaimTrigger makes every insert into upstream_sessions fail,
// or, with skip, insert nothing, until the returned function removes it.
func (f *mcpFixture) addClaimTrigger(t *testing.T, skip bool) func() {
	t.Helper()

	body := "RAISE EXCEPTION 'claim refused'"
	if skip {
		body = "RETURN NULL"
	}

	_, err := f.pool.Exec(t.Context(), "CREATE FUNCTION test_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN "+body+"; END $$")
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), "CREATE TRIGGER test_claim BEFORE INSERT ON upstream_sessions FOR EACH ROW EXECUTE FUNCTION test_claim()")
	require.NoError(t, err)

	return func() {
		_, err := f.pool.Exec(t.Context(), "DROP TRIGGER test_claim ON upstream_sessions")
		require.NoError(t, err)
	}
}

func TestFailedClaimLeavesNoRow(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "user:alice")

	remove := fixture.addClaimTrigger(t, false)
	result := callTool(t, client.session, "github__session_id", nil)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "can't be reached")
	assert.Empty(t, fixture.upstreamSessionRows(t))
	remove()

	id := callSessionID(t, client.session, "github")
	assert.Equal(t, 2, upstream.initializeCount(), "the next forward starts a new upstream session")

	rows := fixture.upstreamSessionRows(t)
	require.Len(t, rows, 1)
	assert.Equal(t, id, rows[0].upstream)
}

func TestLostRaceWithNoLiveRowForwardsOnceAndEnds(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addEnableTemplate("github", "per-user", anyone)
	upstream := fixture.startSessionUpstream(t, "github", sessionUpstreamOptions{})

	client := fixture.connectObserved(t, alice())
	fixture.sessionfulInstance(t, client, "github", "user:alice")

	// The insert returns no row, and no live row exists: as when the
	// winner's row ended between the insert and the read.
	remove := fixture.addClaimTrigger(t, true)
	id := callSessionID(t, client.session, "github")
	assert.Empty(t, fixture.upstreamSessionRows(t))
	require.Eventually(t, func() bool { return len(upstream.deletes()) == 1 }, waitTimeout, 10*time.Millisecond)
	assert.Equal(t, id, upstream.deletes()[0], "the call ran on the new session, which then ended")
	remove()

	next := callSessionID(t, client.session, "github")
	assert.NotEqual(t, id, next, "the next forward claims again")
	assert.Equal(t, 2, upstream.initializeCount())
	assert.Len(t, fixture.upstreamSessionRows(t), 1)
}

func TestRemoteServerUsesOneUpstreamSessionAcrossReplicas(t *testing.T) {
	setUp := func(t *testing.T) (*remoteFixture, *observedClient, *observedClient) {
		t.Helper()

		fixture := newRemoteFixture(t, "alice-key")
		fixture.setSecret(t, "alice", "alice-key")
		other := fixture.replica(t)

		first := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, first, remoteServer, "user:alice", "echo")
		fixture.setRevision(t, instance.ID, sessionfulRevision)
		second := fixture.connectObservedTo(t, other.URL, alice())

		return fixture, first, second
	}

	t.Run("calls through two replicas reach one upstream session with the owner's headers", func(t *testing.T) {
		fixture, first, second := setUp(t)

		for _, client := range []*observedClient{first, second, first} {
			result := callTool(t, client.session, "mail-archiver__echo", map[string]any{"text": "hi"})
			require.False(t, result.IsError, resultText(result))
		}

		sessions := fixture.upstream.callSessions()
		require.Len(t, sessions, 3)
		assert.Equal(t, []string{sessions[0], sessions[0], sessions[0]}, sessions)
		assert.Equal(t, []string{"Bearer alice-key", "Bearer alice-key", "Bearer alice-key"}, fixture.upstream.callAuthorizations())

		rows := fixture.upstreamSessionRows(t)
		require.Len(t, rows, 1)
		assert.Equal(t, sessions[0], rows[0].upstream)
	})

	t.Run("a refused credential releases the copy, keeps the row, and the next copy reads the headers again", func(t *testing.T) {
		fixture, first, second := setUp(t)

		require.False(t, callTool(t, first.session, "mail-archiver__echo", map[string]any{"text": "a"}).IsError)
		require.False(t, callTool(t, second.session, "mail-archiver__echo", map[string]any{"text": "b"}).IsError)

		id := fixture.upstream.callSessions()[0]

		// The server now accepts only a new key, which the user sets.
		fixture.upstream.mu.Lock()
		fixture.upstream.accepted = map[string]bool{"Bearer alice-key-2": true}
		fixture.upstream.mu.Unlock()

		result := callTool(t, second.session, "mail-archiver__echo", map[string]any{"text": "refused"})
		assert.True(t, result.IsError)
		assert.Contains(t, resultText(result), "refused")

		rows := liveRows(fixture.upstreamSessionRows(t))
		require.Len(t, rows, 1, "the row stays")
		assert.Equal(t, id, rows[0].upstream)

		fixture.secrets.mu.Lock()
		fixture.secrets.values["alice"][remoteSecret] = "alice-key-2"
		fixture.secrets.mu.Unlock()

		result = callTool(t, second.session, "mail-archiver__echo", map[string]any{"text": "again"})
		require.False(t, result.IsError, resultText(result))

		sessions := fixture.upstream.callSessions()
		assert.Equal(t, id, sessions[len(sessions)-1], "the new copy adopts the stored session")

		authorizations := fixture.upstream.callAuthorizations()
		assert.Equal(t, "Bearer alice-key-2", authorizations[len(authorizations)-1])
	})
}
