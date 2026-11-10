//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

func TestToolSearch(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	pods := gatewayPods(t, admin)
	endpoint := forwardToGateway(t, admin, pods[0])

	t.Run("with search on the list holds the meta tools and find_tools, a search finds tools of several servers, and a call enables and runs", func(t *testing.T) {
		first := createTemplate(t, kube, "e2e-search-a", "per-session", testServerResources(t, mcpServerImage(t), 0, nil))
		second := createTemplate(t, kube, "e2e-search-b", "per-session", testServerResources(t, mcpServerImage(t), 0, nil))
		declareTools(t, first, "", "echo")
		declareTools(t, second, "", "echo")

		header := http.Header{}
		header.Set(auth.ToolSearchHeader, "on")

		sessionID := "e2e-" + rand.String(8)
		session := connectGatewayWithHeader(t, endpoint, "alice", sessionID, header)

		listed, err := session.session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		names := []string{}
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}

		assert.ElementsMatch(t, []string{
			gateway.ToolListServers, gateway.ToolServerStatus, gateway.ToolFindTools, gateway.ToolEnableServer, gateway.ToolDisableServer,
		}, names)

		var found gateway.FindToolsResult

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: gateway.ToolFindTools, Arguments: map[string]any{"query": "echo", "limit": 20},
			})
			require.NoError(collect, err)

			encoded, err := json.Marshal(result.StructuredContent)
			require.NoError(collect, err)
			require.NoError(collect, json.Unmarshal(encoded, &found))

			servers := map[string]bool{}
			for _, tool := range found.Tools {
				servers[tool.Server] = true
			}

			assert.True(collect, servers[first] && servers[second], "the search rows are built by the leader")
		}, 2*time.Minute, time.Second)

		for _, tool := range found.Tools {
			assert.NotNil(t, tool.InputSchema)
		}

		result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{Name: first + "__echo", Arguments: map[string]any{"text": "hi"}})
		require.NoError(t, err)
		require.False(t, result.IsError, toolText(result))
		requireOneInstance(t, admin, first)

		reasons := gatewayQuery(t, admin, "SELECT string_agg(type || ':' || coalesce(details->>'reason', ''), ',' ORDER BY occurred_at) FROM audit_events WHERE session = '"+
			sessionID+"' AND type IN ('server.enable', 'tool.decision', 'tool.result')")
		assert.True(t, strings.Contains(reasons, "server.enable:first-call") && strings.Contains(reasons, "tool.decision") && strings.Contains(reasons, "tool.result"), reasons)
	})
}
