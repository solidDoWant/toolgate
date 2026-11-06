//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	revisionModern = "2026-07-28"
	revisionLegacy = "2025-11-25"
)

// modernSession is a 2026-07-28 client session with the gateway that
// records its notifications and the URLs it was asked to open.
type modernSession struct {
	gatewaySession

	mu   sync.Mutex
	urls []string
}

func (m *modernSession) elicitedURLs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.urls...)
}

// connectModern connects a go-sdk client on 2026-07-28 that never sends
// server/discover: a sending middleware answers it with only 2026-07-28. So
// the first request the gateway gets carries the 2026-07-28 _meta. With
// onlyModern, the middleware also refuses initialize, as a client that
// speaks nothing older.
func connectModern(t *testing.T, endpoint, user, session string, onlyModern bool) *modernSession {
	t.Helper()

	observed := &modernSession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-modern", Version: "0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			observed.mu.Lock()
			observed.urls = append(observed.urls, request.Params.URL)
			observed.mu.Unlock()

			return &mcp.ElicitResult{Action: "accept"}, nil
		},
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { observed.record(eventListChanged) },
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
			observed.record(eventProgress)
		},
	})
	mcpClient.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			switch {
			case method == "server/discover":
				return &mcp.DiscoverResult{
					SupportedVersions: []string{revisionModern},
					Capabilities:      &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
				}, nil
			case method == "initialize" && onlyModern:
				return nil, errors.New("this client speaks only 2026-07-28")
			}

			return next(ctx, method, request)
		}
	})

	connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{header: gatewayHeader(t, user, session, nil)}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: revisionModern})
	require.NoError(t, err)
	t.Cleanup(func() { _ = connected.Close() })

	observed.session = connected

	return observed
}

// postRaw sends one JSON-RPC request to the gateway with header and returns
// the response and its body.
func postRaw(t *testing.T, endpoint string, header http.Header, message map[string]any) (*http.Response, string) {
	t.Helper()

	body, err := json.Marshal(message)
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	require.NoError(t, err)

	request.Header = header
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response, string(content)
}

// modernRequest is a 2026-07-28 request of method, with the header the
// go-sdk sends and the _meta fields.
func modernRequest(header http.Header, method string) map[string]any {
	header.Set("Mcp-Protocol-Version", revisionModern)
	header.Set("Mcp-Method", method)

	return map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": method,
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    revisionModern,
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "raw", "version": "0"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}},
	}
}

// sessionRows counts the sessions rows of principal.
func sessionRows(t *testing.T, principal string) int {
	t.Helper()

	databaseURL := os.Getenv("TOOLGATE_E2E_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TOOLGATE_E2E_DATABASE_URL is not set")
	}

	pool, err := store.Open(t.Context(), databaseURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var count int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions WHERE principal = $1", principal).Scan(&count))

	return count
}

// approveInBrowser signs in as alice at the approval link and approves it.
func approveInBrowser(t *testing.T, link string) {
	t.Helper()

	browser := uiBrowser(t)

	response, page := openApprovalLink(t, browser, link, testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	require.Equal(t, http.StatusOK, response.StatusCode, page)

	response, err := browser.Get(link)
	require.NoError(t, err)

	page = readPage(t, response)
	token := csrfTokenPattern.FindStringSubmatch(page)
	require.NotNil(t, token, "the approval page shows the approve form: %s", page)

	response, err = browser.PostForm(link+"/approve", url.Values{"csrf_token": {token[1]}})
	require.NoError(t, err)

	page = readPage(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, page)
	require.Contains(t, page, "This approval was approved.")
}

func TestAC4_08ClientRevisions(t *testing.T) {
	admin := adminClient(t)
	name := createApprovalTemplate(t)
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	webUI(t, endpoint)

	t.Run("[AC-4-08] a go-sdk client at its default tries 2026-07-28 first and ends on 2025-11-25", func(t *testing.T) {
		session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		assert.Equal(t, revisionLegacy, session.session.InitializeResult().ProtocolVersion)
	})

	t.Run("[AC-4-08] server/discover without X-Toolgate-Session gets the older revisions and makes no session", func(t *testing.T) {
		principal := "e2e-discover-" + rand.String(6)
		header := gatewayHeader(t, principal, "", nil)
		header.Del(auth.SessionHeader)

		response, body := postRaw(t, endpoint, header, modernRequest(header, "server/discover"))
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		assert.Contains(t, body, revisionLegacy)
		assert.NotContains(t, body, revisionModern)
		assert.Empty(t, response.Header.Get("Mcp-Session-Id"), "no MCP session")
		assert.Zero(t, sessionRows(t, principal), "no sessions row")
	})

	t.Run("[AC-4-08] a 2026-07-28 request without X-Toolgate-Session gets 400 with a plain invalid request error", func(t *testing.T) {
		principal := "e2e-nosession-" + rand.String(6)
		header := gatewayHeader(t, principal, "", nil)
		header.Del(auth.SessionHeader)

		response, body := postRaw(t, endpoint, header, modernRequest(header, "tools/list"))
		require.Equal(t, http.StatusBadRequest, response.StatusCode, body)

		var decoded struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &decoded), body)
		assert.Equal(t, -32600, decoded.Error.Code, "never -32022, so a dual-era client falls back to initialize")
		assert.Equal(t, "This client sends no session. Use the toolgate plugin's headers helper, or a client on MCP 2025-11-25.", decoded.Error.Message)
		assert.Zero(t, sessionRows(t, principal), "no sessions row")
	})

	t.Run("[AC-4-08] a dual-era client with X-Toolgate-Session that sends a modern request first stays on 2026-07-28", func(t *testing.T) {
		session := connectModern(t, endpoint, "alice", "e2e-"+rand.String(8), false)
		assert.Equal(t, revisionModern, session.session.InitializeResult().ProtocolVersion)
		assert.Subset(t, listedTools(t, t.Context(), session.session), []string{gateway.ToolListServers})
	})

	t.Run("[AC-4-08] a 2026-07-28-only client lists, enables, calls, completes an approval and disables", func(t *testing.T) {
		session := connectModern(t, endpoint, "alice", "e2e-"+rand.String(8), true)
		require.Equal(t, revisionModern, session.session.InitializeResult().ProtocolVersion)

		listed, err := session.session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		assert.Equal(t, "private", listed.CacheScope)

		tools := enable(t, &session.gatewaySession, name)
		assert.Contains(t, tools, name+"__echo")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Contains(collect, session.recorded(), eventListChanged)
		}, time.Minute, 100*time.Millisecond, "list_changed arrives through subscriptions/listen")
		assert.Contains(t, session.recorded(), eventProgress, "the enable reports progress while it waits")
		assert.Contains(t, listedTools(t, t.Context(), session.session), name+"__echo")

		calls := startCall(t.Context(), session.session, &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "modern approval"}})

		var link string

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			urls := session.elicitedURLs()
			if assert.NotEmpty(collect, urls, "the call asks the client to open the approval page") {
				link = urls[0]
			}
		}, time.Minute, 100*time.Millisecond)

		approveInBrowser(t, link)

		select {
		case call := <-calls:
			require.NoError(t, call.err)
			require.False(t, call.result.IsError, callText(call.result))
			assert.Contains(t, callText(call.result), "modern approval")
		case <-time.After(time.Minute):
			t.Fatal("the approved call did not return")
		}

		result := callServerTool(t.Context(), session.session, gateway.ToolDisableServer, name)
		require.Empty(t, result.err)
		assert.NotContains(t, listedTools(t, t.Context(), session.session), name+"__echo")
	})
}
