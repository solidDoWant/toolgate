package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// revisionInputRequests is the protocol revision on which the client
// receives the input requests and retries the call itself.
const revisionInputRequests = "2026-07-28"

// elicitationRetry is what the call's retry carried.
type elicitationRetry struct {
	requestState string
	accepted     bool
}

// TestApprovalElicitationInputRequests proves the elicitation step on
// 2026-07-28. The gateway's stateful endpoint negotiates 2025-11-25 at most
// (that revision is proved against the endpoint in the integration tests),
// so the same step is served here through a stateless go-sdk server.
func TestApprovalElicitationInputRequests(t *testing.T) {
	const approvalID = "0190c0de0000700080000000000000aa"

	for _, answer := range []string{"accept", "decline", "cancel"} {
		t.Run(answer, func(t *testing.T) {
			tools := &metaTools{approvalURL: func(id string) string { return "https://toolgate.example/ui/approvals/" + id }}
			call := approvalCall{template: &v1alpha1.MCPServerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "github"}}, tool: "echo"}

			var (
				mu      sync.Mutex
				retries []elicitationRetry
			)

			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
			server.AddTool(&mcp.Tool{Name: "github__echo", InputSchema: map[string]any{"type": "object"}},
				func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if !supportsURLElicitation(ctx, request) {
						return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no elicitation"}}}, nil
					}

					if request.Params.InputResponses == nil {
						return tools.approvalInputRequest(call, approvalID), nil
					}

					retry := elicitationRetry{
						requestState: request.Params.RequestState,
						accepted:     approvalAccepted(request.Params.InputResponses, approvalID),
					}

					mu.Lock()
					defer mu.Unlock()

					retries = append(retries, retry)

					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "retried"}}}, nil
				})

			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
			httpServer := httptest.NewServer(handler)
			t.Cleanup(httpServer.Close)

			var elicited []*mcp.ElicitParams

			client := mcp.NewClient(&mcp.Implementation{Name: "eliciting", Version: "0"}, &mcp.ClientOptions{
				Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
				ElicitationHandler: func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					elicited = append(elicited, request.Params)

					return &mcp.ElicitResult{Action: answer}, nil
				},
			})

			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL},
				&mcp.ClientSessionOptions{ProtocolVersion: revisionInputRequests})
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })

			// The test proves the revision it asks for, not a fallback.
			require.Equal(t, revisionInputRequests, session.InitializeResult().ProtocolVersion)

			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "github__echo", Arguments: map[string]any{}})
			require.NoError(t, err)

			require.Len(t, result.Content, 1)
			assert.Equal(t, "retried", result.Content[0].(*mcp.TextContent).Text)

			require.Len(t, elicited, 1)
			assert.Equal(t, "url", elicited[0].Mode)
			assert.Equal(t, "https://toolgate.example/ui/approvals/"+approvalID, elicited[0].URL)
			assert.Equal(t, approvalID, elicited[0].ElicitationID)

			mu.Lock()
			defer mu.Unlock()

			// One retry: a request with responses never gets input
			// requests back.
			assert.Equal(t, []elicitationRetry{{requestState: approvalID, accepted: answer == "accept"}}, retries)
		})
	}
}
