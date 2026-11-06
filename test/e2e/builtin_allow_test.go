//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// allowingSession is an MCP session whose client declares URL elicitation,
// accepts every elicitation, and keeps the URLs it was asked to open.
type allowingSession struct {
	session *mcp.ClientSession

	mu   sync.Mutex
	urls []string
}

func (s *allowingSession) elicitedURLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.urls)
}

// connectAllowing opens an MCP session with header and enables the
// built-in server in it, so its tools go through the go-sdk's tool path.
func connectAllowing(t *testing.T, endpoint string, header http.Header) *allowingSession {
	t.Helper()

	observed := &allowingSession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-allowing", Version: "0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			observed.mu.Lock()
			observed.urls = append(observed.urls, request.Params.URL)
			observed.mu.Unlock()

			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})

	connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{header: header}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connected.Close() })

	observed.session = connected

	result, err := connected.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolEnableServer, Arguments: map[string]any{"name": gateway.BuiltinServerName}})
	require.NoError(t, err)
	require.False(t, result.IsError, callText(result))
	require.Contains(t, listedTools(t, t.Context(), connected), "toolgate_allow")

	return observed
}

// listScopes calls toolgate_list_scopes.
func listScopes(t *testing.T, session *mcp.ClientSession) gateway.ListScopesResult {
	t.Helper()

	var scopes gateway.ListScopesResult
	structuredResult(t, session, "toolgate_list_scopes", &scopes)

	return scopes
}

// approveScopeInBrowser signs in to the web UI as alice in a new browser,
// which is a fresh sign-in, so the approval's step-up passes, then opens the
// approval link and approves.
func approveScopeInBrowser(t *testing.T, publicURL, link string) {
	t.Helper()

	browser := signInToUI(t, publicURL, testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}})

	approve := `form[action$="/approve"] button[type="submit"]`
	require.NoError(t, chromedp.Do(browser, testutil.Navigate(link), chromedp.WaitVisible(approve), chromedp.Click(approve)))

	require.Eventually(t, func() bool {
		body, err := chromedp.Run(browser, testutil.BodyText())

		return err == nil && strings.Contains(body, "approved")
	}, time.Minute, 200*time.Millisecond, "the page shows the request approved")
}

// allowOutcome is the end of a toolgate_allow call.
type allowOutcome struct {
	result gateway.AllowResult
	err    error
}

// startAllow calls toolgate_allow in the background.
func startAllow(ctx context.Context, session *mcp.ClientSession, arguments map[string]any) <-chan allowOutcome {
	outcomes := make(chan allowOutcome, 1)

	go func() {
		var result gateway.AllowResult

		response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "toolgate_allow", Arguments: arguments})
		if err == nil && response.IsError {
			err = &allowError{message: callText(response)}
		}

		if err == nil {
			encoded, _ := json.Marshal(response.StructuredContent)
			err = json.Unmarshal(encoded, &result)
		}

		outcomes <- allowOutcome{result: result, err: err}
	}()

	return outcomes
}

type allowError struct{ message string }

func (e *allowError) Error() string { return e.message }

func scopeOfWorkspace(scopes gateway.ListScopesResult, workspace string) (gateway.WorkspaceSummary, bool) {
	for _, each := range scopes.Workspaces {
		if each.Workspace == workspace {
			return each, true
		}
	}

	return gateway.WorkspaceSummary{}, false
}

func scopeOfSession(scopes gateway.ListScopesResult, session string) (gateway.SessionSummary, bool) {
	for _, each := range scopes.Sessions {
		if each.Session == session {
			return each, true
		}
	}

	return gateway.SessionSummary{}, false
}

func TestAC4_15ToolgateAllow(t *testing.T) {
	namespace := workloadNamespace(t)
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	server := attestationTemplate(t, kube, "e2e-allow", "per-user",
		v1alpha1.Access{Users: []string{"alice"}, Environments: "any"}, testServerResources(t, image, 0, nil))

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	publicURL := webUI(t, endpoint)

	token := workspacePodToken(t, namespace, image, "alice", "dev")
	caller := connectAllowing(t, endpoint, attestedHeader(t, "alice", nil, token, ""))

	targetHeader := attestedHeader(t, "alice", nil, token, "")
	target := targetHeader.Get(auth.SessionHeader)
	connectWithHeader(t, endpoint, targetHeader)

	for _, test := range []struct {
		name      string
		arguments map[string]any
	}{
		{
			name:      "another workspace",
			arguments: map[string]any{"workspace": "coder/alice/e2e-allow-" + rand.String(5), "servers": []string{server}},
		},
		{
			name:      "another session",
			arguments: map[string]any{"session": target, "servers": []string{server}},
		},
	} {
		t.Run("[AC-4-15] toolgate_allow for "+test.name+" returns a link and an elicitation, and applies only after the step-up approval", func(t *testing.T) {
			applied := func(scopes gateway.ListScopesResult) bool {
				if workspace, ok := test.arguments["workspace"].(string); ok {
					scope, found := scopeOfWorkspace(scopes, workspace)

					return found && slices.Contains(scope.Servers, server)
				}

				scope, found := scopeOfSession(scopes, target)

				return found && slices.Contains(scope.Allowed, server)
			}

			before := len(caller.elicitedURLs())
			outcomes := startAllow(t.Context(), caller.session, test.arguments)

			require.Eventually(t, func() bool { return len(caller.elicitedURLs()) > before }, time.Minute, 100*time.Millisecond,
				"the call asks the client to open the approval page")

			link := caller.elicitedURLs()[before]
			assert.True(t, strings.HasPrefix(link, publicURL+"/ui/approvals/"), link)

			// Nothing changes before the approval.
			assert.False(t, applied(listScopes(t, caller.session)), "the change applied before the approval")

			approveScopeInBrowser(t, publicURL, link)

			select {
			case outcome := <-outcomes:
				require.NoError(t, outcome.err)
				assert.Equal(t, "approved", outcome.result.Status)
				assert.Equal(t, link, outcome.result.URL)
			case <-time.After(time.Minute):
				require.FailNow(t, "toolgate_allow didn't return after the approval")
			}

			assert.True(t, applied(listScopes(t, caller.session)), "the change applies after the approval")
		})
	}

	t.Run("[AC-4-15] the session-scoped allow is gone after that session ends", func(t *testing.T) {
		require.Equal(t, "1", strings.TrimSpace(gatewayQuery(t, admin, "SELECT count(*) FROM session_additions a JOIN sessions s ON s.id = a.session_pk "+
			"WHERE s.principal = 'alice' AND s.session_id = '"+target+"' AND s.ended_at IS NULL")), "the approved allow is in place")

		idleOut(t, admin, target)

		assert.Equal(t, "0", strings.TrimSpace(gatewayQuery(t, admin, "SELECT count(*) FROM session_additions a JOIN sessions s ON s.id = a.session_pk "+
			"WHERE s.principal = 'alice' AND s.session_id = '"+target+"'")))

		_, found := scopeOfSession(listScopes(t, caller.session), target)
		assert.False(t, found, "the ended session isn't listed")
	})
}
