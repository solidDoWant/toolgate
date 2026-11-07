//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

// curatedTemplate creates a template for the test MCP server with the tools
// echo and other, and applies change to its spec.
func curatedTemplate(t *testing.T, kube client.Client, change func(*v1alpha1.MCPServerTemplateSpec)) string {
	t.Helper()

	name := "e2e-curate-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice"}, Environments: "any"},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    testServerResourcesWithTools(t, mcpServerImage(t), []string{"echo", "other"}, 0, nil),
		},
	}
	change(&template.Spec)

	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })

	return name
}

// upstreamCalls returns how many times each tool was called on the test MCP
// server of the template's only instance.
func upstreamCalls(t *testing.T, admin *kubernetes.Clientset, template string) map[string]int {
	t.Helper()

	namespace := requireOneInstance(t, admin, template)

	body, err := admin.CoreV1().Services(namespace.Name).ProxyGet("http", "mcp", "8080", "calls", nil).DoRaw(t.Context())
	require.NoError(t, err)

	var counts map[string]int
	require.NoError(t, json.Unmarshal(body, &counts))

	return counts
}

// toolCall calls a tool and returns its text and whether it is an error.
func toolCall(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) (string, bool) {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)

	var text strings.Builder

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text.WriteString(textContent.Text)
		}
	}

	return text.String(), result.IsError
}

func TestToolFiltering(t *testing.T) {
	kube := templateClient(t)
	admin := adminClient(t)

	allowList := curatedTemplate(t, kube, func(spec *v1alpha1.MCPServerTemplateSpec) {
		spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"echo"}}
	})
	denyList := curatedTemplate(t, kube, func(spec *v1alpha1.MCPServerTemplateSpec) {
		spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"other"}}
	})
	requireTemplateValid(t, kube, allowList)
	requireTemplateValid(t, kube, denyList)

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	for _, server := range []string{allowList, denyList} {
		t.Run("hidden tools of "+server+" are absent and refused by name, and never forwarded", func(t *testing.T) {
			session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
			enable(t, session, server)

			listed := listedTools(t, t.Context(), session.session)
			assert.Contains(t, listed, server+"__echo")
			assert.NotContains(t, listed, server+"__other")

			text, isError := toolCall(t, session.session, server+"__other", map[string]any{})
			assert.True(t, isError)
			assert.Equal(t, "Server `"+server+"` has no tool `other`.", text)

			assert.Zero(t, upstreamCalls(t, admin, server)["other"], "the hidden tool was not forwarded")
		})
	}
}

func TestToolOverrides(t *testing.T) {
	kube := templateClient(t)
	admin := adminClient(t)

	server := curatedTemplate(t, kube, func(spec *v1alpha1.MCPServerTemplateSpec) {
		spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {
			Name: "say", Description: "Say it back.", InputDescriptions: map[string]string{"text": "What to say."},
		}}
	})
	requireTemplateValid(t, kube, server)

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	t.Run("the client sees the override and the upstream receives its own name", func(t *testing.T) {
		session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		enable(t, session, server)

		listed, err := session.session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		var names []string

		for _, tool := range listed.Tools {
			names = append(names, tool.Name)

			if tool.Name == server+"__say" {
				assert.Equal(t, "Say it back.", tool.Description)
			}
		}

		assert.Contains(t, names, server+"__say")
		assert.NotContains(t, names, server+"__echo")

		text, isError := toolCall(t, session.session, server+"__say", map[string]any{"text": "hi"})
		require.False(t, isError, text)
		assert.Contains(t, text, `"tool":"echo"`)

		text, isError = toolCall(t, session.session, server+"__echo", map[string]any{"text": "hi"})
		assert.True(t, isError)
		assert.Equal(t, "Server `"+server+"` has no tool `echo`.", text)

		assert.Equal(t, 1, upstreamCalls(t, admin, server)["echo"], "only the renamed call reached the upstream")
	})

	t.Run("an override name over the 64-character limit makes the template invalid", func(t *testing.T) {
		long := curatedTemplate(t, kube, func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Name: strings.Repeat("a", 60)}}
		})

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			var template v1alpha1.MCPServerTemplate
			require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: long}, &template))
			assert.True(collect, meta.IsStatusConditionFalse(template.Status.Conditions, catalog.ConditionValid), "%+v", template.Status.Conditions)
		}, 2*time.Minute, time.Second)
	})
}
