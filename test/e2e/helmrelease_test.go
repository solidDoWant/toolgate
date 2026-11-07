//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

// chartRepository returns the OCI repository of the test chart, as Flux in
// the cluster reaches it.
func chartRepository(t *testing.T) string {
	t.Helper()

	repository := os.Getenv("TOOLGATE_E2E_CHART_REPOSITORY")
	if repository == "" {
		t.Skip("TOOLGATE_E2E_CHART_REPOSITORY is not set (run through scripts/e2e/run.sh)")
	}

	return repository
}

// helmReleaseResources are an OCIRepository with the test chart and a
// HelmRelease that installs it with values. The chart runs the test MCP
// server as the Service mcp, and refuses to install without the value image.
func helmReleaseResources(t *testing.T, values map[string]any) []k8sruntime.RawExtension {
	t.Helper()

	repository := map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "OCIRepository",
		"metadata":   map[string]any{"name": "mcpserver"},
		"spec": map[string]any{
			"url":      chartRepository(t),
			"insecure": true,
			"interval": "10m",
			"ref":      map[string]any{"tag": "0.1.0"},
		},
	}
	release := map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata":   map[string]any{"name": "mcpserver"},
		"spec": map[string]any{
			"interval": "10m",
			"chartRef": map[string]any{"kind": "OCIRepository", "name": "mcpserver"},
			"values":   values,
		},
	}

	var resources []k8sruntime.RawExtension

	for _, object := range []map[string]any{repository, release} {
		raw, err := json.Marshal(object)
		require.NoError(t, err)

		resources = append(resources, k8sruntime.RawExtension{Raw: raw})
	}

	return resources
}

// requireNamespaceDeleted waits until the namespace no longer exists, so
// every object in it, those with finalizers included, has been removed.
func requireNamespaceDeleted(t *testing.T, admin *kubernetes.Clientset, name string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		_, err := admin.CoreV1().Namespaces().Get(t.Context(), name, metav1.GetOptions{})
		assert.True(collect, apierrors.IsNotFound(err), "namespace %s still exists (err %v)", name, err)
	}, 4*time.Minute, 2*time.Second)
}

func TestHelmReleaseTemplate(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	values := map[string]any{"image": image, "tools": strings.Join(testServerTools, ",")}
	name := createTemplate(t, kube, "e2e-helm", "per-session", helmReleaseResources(t, values))
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

	var namespace string

	t.Run("the template becomes ready when Flux reports the release ready, and serves tools", func(t *testing.T) {
		tools := enable(t, session, name)
		assert.ElementsMatch(t, prefixedTools(name), tools)

		namespace = requireOneInstance(t, admin, name).Name

		release := helmReleaseOf(t, namespace)
		assert.True(t, conditionIs(release, "Ready", "True"), "the HelmRelease is ready: %v", release)

		result := callServerTool(t.Context(), session.session, name+"__echo", name)
		assert.Empty(t, result.err, "the server's tool answers")
	})

	t.Run("stopping the instance removes it fully", func(t *testing.T) {
		require.NotEmpty(t, namespace, "the enable subtest found the namespace")

		result := callServerTool(t.Context(), session.session, gateway.ToolDisableServer, name)
		require.Empty(t, result.err)

		requireNamespaceDeleted(t, admin, namespace)
	})
}

func TestFailingHelmRelease(t *testing.T) {
	mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	// Without the value image the chart doesn't render, so the install fails.
	name := createTemplate(t, kube, "e2e-helmfail", "per-session", helmReleaseResources(t, map[string]any{}))
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

	t.Run("a HelmRelease that fails to install fails toolgate_enable_server with its reason", func(t *testing.T) {
		started := time.Now()
		result := awaitEnable(t, startEnable(t, session, name))

		assert.Contains(t, result.err, "HelmRelease mcpserver failed")
		assert.Contains(t, result.err, "the value image is required")
		assert.Less(t, time.Since(started), enableTimeout, "the failure is reported before the start timeout")
	})
}

// helmReleaseOf returns the HelmRelease mcpserver in namespace as a map.
func helmReleaseOf(t *testing.T, namespace string) map[string]any {
	t.Helper()

	raw, err := adminClient(t).RESTClient().Get().
		AbsPath("/apis/helm.toolkit.fluxcd.io/v2/namespaces", namespace, "helmreleases", "mcpserver").
		DoRaw(t.Context())
	require.NoError(t, err)

	var release map[string]any
	require.NoError(t, json.Unmarshal(raw, &release))

	return release
}

// conditionIs reports whether object has the condition kind with status.
func conditionIs(object map[string]any, kind, status string) bool {
	statusField, _ := object["status"].(map[string]any)
	conditions, _ := statusField["conditions"].([]any)

	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if condition["type"] == kind {
			return condition["status"] == status
		}
	}

	return false
}
