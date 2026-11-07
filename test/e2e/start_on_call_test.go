//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

// declareTools sets the template's declared tools and start mode.
func declareTools(t *testing.T, name, start string, tools ...string) {
	t.Helper()

	kube := templateClient(t)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var template v1alpha1.MCPServerTemplate
		require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))

		template.Spec.Start = start
		template.Spec.DeclaredTools = nil

		for _, tool := range tools {
			template.Spec.DeclaredTools = append(template.Spec.DeclaredTools, v1alpha1.DeclaredTool{
				Name: tool, InputSchema: &k8sruntime.RawExtension{Raw: []byte(`{"type":"object"}`)},
			})
		}

		require.NoError(collect, kube.Update(t.Context(), &template))
	}, time.Minute, time.Second, "declare tools")

	requireTemplateValid(t, kube, name)
}

func TestStartOnFirstCall(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	pods := gatewayPods(t, admin)
	endpoint := forwardToGateway(t, admin, pods[0])

	t.Run("a call to a cataloged server with nothing running starts it, sends progress and is forwarded", func(t *testing.T) {
		name := createTemplate(t, kube, "e2e-oncall", "per-session", testServerResources(t, mcpServerImage(t), 0, nil))
		declareTools(t, name, "", "echo")

		session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		require.Equal(t, []string{name + "__echo"}, enable(t, session, name), "the enable returns the catalog list")
		assert.Empty(t, instanceNamespaces(t, t.Context(), admin, name), "nothing starts on enable")

		before := len(session.recorded())

		result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: name + "__echo", Arguments: map[string]any{"text": "hello"}, Meta: mcp.Meta{"progressToken": "call-1"},
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		assert.Contains(t, session.recorded()[before:], eventProgress)
		requireOneInstance(t, admin, name)
	})

	t.Run("a start failure fails the call with its reason", func(t *testing.T) {
		name := createTemplate(t, kube, "e2e-oncall-fail", "per-session", helmReleaseResources(t, map[string]any{}))
		declareTools(t, name, "", "echo")

		session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		enable(t, session, name)

		result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "x"}})
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.Contains(t, toolText(result), "the value image is required")
	})

	t.Run("a template with start onEnable starts on enable", func(t *testing.T) {
		name := createTemplate(t, kube, "e2e-onenable", "per-session", testServerResources(t, mcpServerImage(t), 0, nil))
		declareTools(t, name, catalog.StartOnEnable, "echo", "env")

		session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		enable(t, session, name)
		requireOneInstance(t, admin, name)
	})
}

func toolText(result *mcp.CallToolResult) string {
	text := ""

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}

	return text
}
