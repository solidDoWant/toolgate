//go:build e2e

package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

// startDelay holds the test server unready, so a session can list tools
// while the instance is still starting.
const startDelay = 20 * time.Second

func TestAC4_12ToolCatalog(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	pods := gatewayPods(t, admin)
	endpoint := forwardToGateway(t, admin, pods[0])

	t.Run("[AC-4-12] declared tools are listed while the instance starts; the real list follows with list_changed", func(t *testing.T) {
		name := createTemplate(t, kube, "e2e-catalog-declared", "per-user", testServerResources(t, mcpServerImage(t), startDelay, nil))

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			var template v1alpha1.MCPServerTemplate
			require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))

			template.Spec.Start = catalog.StartOnEnable
			template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{
				Name: "declared", Description: "A declared tool.",
				InputSchema: &k8sruntime.RawExtension{Raw: []byte(`{"type":"object"}`)},
			}}
			require.NoError(collect, kube.Update(t.Context(), &template))
		}, time.Minute, time.Second, "declare a tool")

		requireTemplateValid(t, kube, name)

		session := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		results := startEnable(t, session, name)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Equal(collect, []string{name + "__declared"}, listedToolsOf(t, session, name))
		}, time.Minute, time.Second, "the declared tools are listed while the instance starts")

		before := len(session.recorded())

		require.Empty(t, awaitEnable(t, results).err)
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.ElementsMatch(collect, prefixedTools(name), listedToolsOf(t, session, name))
		}, time.Minute, time.Second, "the real list replaces the declared one")
		assert.Eventually(t, func() bool {
			return slices.Contains(session.recorded()[before:], eventListChanged)
		}, time.Minute, 500*time.Millisecond, "the session gets list_changed")
	})

	t.Run("[AC-4-12] a learned list is listed to another user while their instance starts", func(t *testing.T) {
		name := createTemplate(t, kube, "e2e-catalog-learned", "per-user", testServerResources(t, mcpServerImage(t), startDelay, nil))
		requireTemplateStartsOnEnable(t, kube, name)

		alice := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, alice, name))

		bob := connectGateway(t, endpoint, "bob", "e2e-"+rand.String(8))
		results := startEnable(t, bob, name)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.ElementsMatch(collect, prefixedTools(name), listedToolsOf(t, bob, name))
		}, startDelay, 500*time.Millisecond, "the learned list is listed before the instance is ready")

		require.Empty(t, awaitEnable(t, results).err)
	})
}

// requireTemplateStartsOnEnable makes the template start its instance on
// enable and waits until it is valid.
func requireTemplateStartsOnEnable(t *testing.T, kube client.Client, name string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var template v1alpha1.MCPServerTemplate
		require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))

		template.Spec.Start = catalog.StartOnEnable
		require.NoError(collect, kube.Update(t.Context(), &template))
	}, time.Minute, time.Second, "start on enable")

	requireTemplateValid(t, kube, name)
}
