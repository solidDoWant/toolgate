//go:build integration

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

const modernRevision = "2026-07-28"

// connectModernTo connects a client that speaks only 2026-07-28 and skips
// server/discover: a sending middleware answers the discover itself with
// only 2026-07-28 and refuses initialize, so the gateway never sees either.
func (f *mcpFixture) connectModernTo(t *testing.T, endpoint string, client user) *observedClient {
	t.Helper()

	observed := &observedClient{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "modern", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			observed.mu.Lock()
			observed.listChanged++
			observed.mu.Unlock()
		},
		ProgressNotificationHandler: func(_ context.Context, request *mcp.ProgressNotificationClientRequest) {
			observed.mu.Lock()
			observed.progress = append(observed.progress, request.Params.Message)
			observed.mu.Unlock()
		},
	})
	mcpClient.AddSendingMiddleware(skipDiscover)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: headerTransport{header: f.headers(t, client)}},
	}

	session, err := mcpClient.Connect(t.Context(), transport, &mcp.ClientSessionOptions{ProtocolVersion: modernRevision})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	observed.session = session

	return observed
}

func skipDiscover(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		switch method {
		case "server/discover":
			return &mcp.DiscoverResult{
				SupportedVersions: []string{modernRevision},
				Capabilities:      &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
			}, nil
		case "initialize":
			return nil, errors.New("this client speaks only 2026-07-28")
		}

		return next(ctx, method, request)
	}
}

// postModern sends one raw 2026-07-28 request, with header changing the
// fixture's headers for client.
func (f *mcpFixture) postModern(t *testing.T, client user, method string, change func(http.Header)) *http.Response {
	t.Helper()

	header := f.headers(t, client)
	header.Set("Mcp-Protocol-Version", modernRevision)
	header.Set("Mcp-Method", method)

	if change != nil {
		change(header)
	}

	message := map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": method,
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    modernRevision,
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "raw", "version": "0"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}},
	}

	return f.postWith(t, header, message)
}

func (f *mcpFixture) countSessions(t *testing.T) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&count))

	return count
}

func TestStatelessClient(t *testing.T) {
	t.Run("a 2026-07-28 client lists, enables, calls and disables on the stateless handler", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectModernTo(t, fixture.server.URL, alice())
		assert.Equal(t, modernRevision, client.session.InitializeResult().ProtocolVersion)

		listed, err := client.session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		assert.Equal(t, "private", listed.CacheScope, "tool lists are never shared")
		assert.Zero(t, listed.TTLMs)

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))
		require.Eventually(t, func() bool { return client.listChangedCount() > 0 }, waitTimeout, 10*time.Millisecond,
			"the enable sends list_changed on the listen stream")
		assert.NotEmpty(t, client.progressMessages(), "the enable reports progress while it waits")

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "modern"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "modern", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1, "the call is decided by policy")
		assert.Equal(t, "allow", decisions[0].Details["decision"])

		disabled, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: gateway.ToolDisableServer, Arguments: map[string]any{"name": "github"},
		})
		require.NoError(t, err)
		assert.False(t, disabled.IsError, "disable: %v", disabled.Content)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))
	})

	t.Run("a 2026-07-28 request without X-Toolgate-Session gets 400 with a plain invalid request error", func(t *testing.T) {
		fixture := newMCPFixture(t)

		response := fixture.postModern(t, alice(), "tools/list", func(header http.Header) { header.Del(auth.SessionHeader) })
		require.Equal(t, http.StatusBadRequest, response.StatusCode)

		var body struct {
			ID    int `json:"id"`
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		assert.Equal(t, 7, body.ID)
		assert.Equal(t, -32600, body.Error.Code, "never -32022, so a dual-era client falls back to initialize")
		assert.Equal(t, "This client sends no session. Use the toolgate plugin's headers helper, or a client on MCP 2025-11-25.", body.Error.Message)
		assert.Zero(t, fixture.countSessions(t))
	})

	t.Run("a 2026-07-28 request without a token gets 401", func(t *testing.T) {
		fixture := newMCPFixture(t)

		response := fixture.postModern(t, alice(), "tools/list", func(header http.Header) { header.Del("Authorization") })
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	})

	t.Run("server/discover without X-Toolgate-Session lists the older revisions and makes no session", func(t *testing.T) {
		fixture := newMCPFixture(t)

		response := fixture.postModern(t, alice(), "server/discover", func(header http.Header) { header.Del(auth.SessionHeader) })
		require.Equal(t, http.StatusOK, response.StatusCode)
		assert.Empty(t, response.Header.Get("Mcp-Session-Id"), "no MCP session is bound")

		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		assert.Contains(t, string(raw), "2025-11-25")
		assert.NotContains(t, string(raw), modernRevision)

		// A server/discover without the 2026-07-28 header is exempt too:
		// it gets the go-sdk's answer, not a 400 or a 401, and no session.
		header := fixture.headers(t, alice())
		header.Del(auth.SessionHeader)
		bare := fixture.postWith(t, header, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover"})
		require.Equal(t, http.StatusOK, bare.StatusCode)
		assert.Empty(t, bare.Header.Get("Mcp-Session-Id"), "no MCP session is bound")

		assert.Zero(t, fixture.countSessions(t), "no sessions row")
	})

	t.Run("server/discover still needs a token", func(t *testing.T) {
		fixture := newMCPFixture(t)

		response := fixture.postModern(t, alice(), "server/discover", func(header http.Header) {
			header.Del(auth.SessionHeader)
			header.Del("Authorization")
		})
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	})

	t.Run("a go-sdk client at its default ends on 2025-11-25", func(t *testing.T) {
		fixture := newMCPFixture(t)

		session := fixture.connect(t, alice())
		assert.Equal(t, "2025-11-25", session.InitializeResult().ProtocolVersion)
	})

	t.Run("a stateless binding is held while its listen stream is open and released when idle", func(t *testing.T) {
		const idle = 300 * time.Millisecond

		fixture := newMCPFixture(t, func(options *gateway.MCPOptions) { options.MCPSessionIdleTimeout = idle })
		handler := fixture.handlers[0]

		client := fixture.connectModernTo(t, fixture.server.URL, alice())
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))
		assert.Equal(t, 1, gateway.StatelessBindings(handler), "one binding for the session's requests")

		// The listen stream keeps the binding past the idle timeout.
		time.Sleep(3 * idle)
		assert.Equal(t, 1, gateway.StatelessBindings(handler), "the open listen stream keeps the binding")

		require.NoError(t, client.session.Close())
		require.Eventually(t, func() bool { return gateway.StatelessBindings(handler) == 0 }, waitTimeout, 10*time.Millisecond,
			"the binding is released once idle")
	})

	t.Run("list_changed reaches a listen stream on another replica", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		other := fixture.replica(t)

		listening := fixture.connectModernTo(t, fixture.server.URL, alice())
		assert.ElementsMatch(t, metaToolNames, toolNames(t, listening.session))
		before := listening.listChangedCount()

		calling := fixture.connectModernTo(t, other.URL, alice())
		fixture.enableReady(t, calling, "github", "user:alice")

		require.Eventually(t, func() bool { return listening.listChangedCount() > before }, waitTimeout, 10*time.Millisecond,
			"replica A sends list_changed for a change made on replica B")
		assert.ElementsMatch(t, append(metaToolNames, "github__search"), toolNames(t, listening.session))
	})
}

// postWith sends one raw MCP message with header.
func (f *mcpFixture) postWith(t *testing.T, header http.Header, message map[string]any) *http.Response {
	t.Helper()

	body, err := json.Marshal(message)
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL, bytes.NewReader(body))
	require.NoError(t, err)

	request.Header = header
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}
