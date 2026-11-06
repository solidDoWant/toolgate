//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

// connectorSession is a 2025-11-25 client session with the gateway that
// sends no X-Toolgate-Session, as a claude.ai connector does. It records
// its notifications and the URLs it was asked to open.
type connectorSession struct {
	gatewaySession

	mu   sync.Mutex
	urls []string
}

func (c *connectorSession) elicitedURLs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.urls...)
}

// toolgateSession is the toolgate session ID the gateway ties to the MCP
// session.
func (c *connectorSession) toolgateSession() string {
	return auth.MCPSessionPrefix + c.session.ID()
}

// connectorHeader is gatewayHeader without the session header.
func connectorHeader(t *testing.T, user string) http.Header {
	t.Helper()

	header := gatewayHeader(t, user, "", nil)
	header.Del(auth.SessionHeader)

	return header
}

func connectConnector(t *testing.T, endpoint, user string) *connectorSession {
	t.Helper()

	observed := &connectorSession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-connector", Version: "0"}, &mcp.ClientOptions{
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

	connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{header: connectorHeader(t, user)}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connected.Close() })

	observed.session = connected

	return observed
}

// initializeConnector starts an MCP session without the session header
// with a raw initialize and returns its Mcp-Session-Id. It opens no stream,
// so the session idles out unless more requests come.
func initializeConnector(t *testing.T, endpoint, user string) string {
	t.Helper()

	response, body := postRaw(t, endpoint, connectorHeader(t, user), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": revisionLegacy,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "raw", "version": "0"},
		},
	})
	require.Equal(t, http.StatusOK, response.StatusCode, body)

	id := response.Header.Get("Mcp-Session-Id")
	require.Regexp(t, `^[0-9a-f]{32}$`, id)

	return id
}

func TestAC4_10SessionsWithoutHelper(t *testing.T) {
	admin := adminClient(t)
	name := createApprovalTemplate(t)
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	webUI(t, endpoint)

	sessionEnds := func(session string) string {
		return strings.TrimSpace(gatewayQuery(t, admin, "SELECT details->>'reason' FROM audit_events WHERE session = '"+
			session+"' AND type = 'session.end'"))
	}

	t.Run("[AC-4-10] a server/discover before initialize gets the older revisions and makes no session", func(t *testing.T) {
		principal := "e2e-connector-discover-" + rand.String(6)
		header := connectorHeader(t, principal)

		response, body := postRaw(t, endpoint, header, modernRequest(header, "server/discover"))
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		assert.Contains(t, body, revisionLegacy)
		assert.NotContains(t, body, revisionModern)
		assert.Empty(t, response.Header.Get("Mcp-Session-Id"), "no MCP session")
		assert.Zero(t, sessionRows(t, principal), "no sessions row")
	})

	t.Run("[AC-4-10] a client without the session header lists, enables, calls, completes an approval and disables", func(t *testing.T) {
		session := connectConnector(t, endpoint, "alice")
		require.Equal(t, revisionLegacy, session.session.InitializeResult().ProtocolVersion)

		assert.Contains(t, listedTools(t, t.Context(), session.session), gateway.ToolListServers)
		assert.Equal(t, "claimed/default", strings.TrimSpace(gatewayQuery(t, admin,
			"SELECT workspace FROM sessions WHERE principal = 'alice' AND session_id = '"+session.toolgateSession()+"'")))

		tools := enable(t, &session.gatewaySession, name)
		assert.Contains(t, tools, name+"__echo")

		calls := startCall(t.Context(), session.session, &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "connector approval"}})

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
			assert.Contains(t, callText(call.result), "connector approval")
		case <-time.After(time.Minute):
			t.Fatal("the approved call did not return")
		}

		result := callServerTool(t.Context(), session.session, gateway.ToolDisableServer, name)
		require.Empty(t, result.err)
		assert.NotContains(t, listedTools(t, t.Context(), session.session), name+"__echo")
	})

	t.Run("[AC-4-10] two clients without the session header get separate sessions", func(t *testing.T) {
		first := connectConnector(t, endpoint, "alice")
		second := connectConnector(t, endpoint, "alice")
		require.NotEqual(t, first.toolgateSession(), second.toolgateSession())

		enable(t, &first.gatewaySession, name)

		assert.Contains(t, listedTools(t, t.Context(), first.session), name+"__echo")
		assert.NotContains(t, listedTools(t, t.Context(), second.session), name+"__echo",
			"the second session has its own enabled set")
	})

	t.Run("[AC-4-10] a DELETE of the MCP session ends the session with session.end", func(t *testing.T) {
		mcpSessionID := initializeConnector(t, endpoint, "alice")

		request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, endpoint, nil)
		require.NoError(t, err)

		request.Header = connectorHeader(t, "alice")
		request.Header.Set("Mcp-Session-Id", mcpSessionID)

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		require.Equal(t, http.StatusNoContent, response.StatusCode)

		assert.Equal(t, "mcp-session-delete", sessionEnds(auth.MCPSessionPrefix+mcpSessionID),
			"the session ends at once, before the DELETE returns")
	})

	t.Run("[AC-4-10] the idle release of the MCP session ends the session with session.end", func(t *testing.T) {
		mcpSessionID := initializeConnector(t, endpoint, "alice")

		require.Eventually(t, func() bool {
			return sessionEnds(auth.MCPSessionPrefix+mcpSessionID) == "mcp-session-idle"
		}, scaled(30*time.Minute)+time.Minute, 5*time.Second)
	})
}
