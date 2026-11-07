//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// revisionTemplate creates a template for the test MCP server in mode, with
// the tool echo.
func revisionTemplate(t *testing.T, kube client.Client, mode string) string {
	t.Helper()

	name := "e2e-rev-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice"}, Environments: "any"},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources: testServerResourcesWithTools(t, mcpServerImage(t), []string{"echo"}, 0,
				map[string]string{"MCPSERVER_MODE": mode}),
		},
	}

	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })

	return name
}

// replaceUpstreamTools sets the tool list of the template's only instance
// to the echo tools names, through the test MCP server's PUT /tools.
func replaceUpstreamTools(t *testing.T, admin *kubernetes.Clientset, template, names string) {
	t.Helper()

	namespace := requireOneInstance(t, admin, template)

	_, err := admin.CoreV1().RESTClient().Put().Namespace(namespace.Name).Resource("services").
		Name("http:mcp:8080").SubResource("proxy").Suffix("tools").Param("names", names).DoRaw(t.Context())
	require.NoError(t, err)
}

func TestUpstreamRevisions(t *testing.T) {
	kube := templateClient(t)
	admin := adminClient(t)
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	tests := []struct {
		mode     string
		revision string
	}{
		{mode: "2025-only", revision: runtime.RevisionStateful},
		{mode: "both", revision: runtime.RevisionStateful},
		{mode: "2026-only", revision: runtime.RevisionStateless},
	}

	for _, test := range tests {
		t.Run("an upstream that speaks "+test.mode+" is served on "+test.revision, func(t *testing.T) {
			server := revisionTemplate(t, kube, test.mode)
			requireTemplateValid(t, kube, server)

			session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
			enable(t, session, server)

			readyRevision := func() string {
				return gatewayQuery(t, admin, "SELECT mcp_revision FROM instances WHERE template_name = '"+server+
					"' AND status = 'ready'")
			}
			assert.Equal(t, test.revision, readyRevision())

			text, isError := toolCall(t, session.session, server+"__echo", map[string]any{"text": "hi"})
			require.False(t, isError, text)
			assert.Contains(t, text, `"tool":"echo"`)

			replaceUpstreamTools(t, admin, server, "echo,added")

			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				assert.Contains(collect, listedTools(t, t.Context(), session.session), server+"__added")
			}, scaledHealthInterval()+time.Minute, time.Second, "the tool-list change reaches the open session")
			assert.Contains(t, session.recorded(), eventListChanged)

			// The monitor, which re-listed the change, still finds the
			// instance healthy, on the same revision.
			assert.Equal(t, test.revision, readyRevision(), "the instance stays ready on its revision")
		})
	}
}
